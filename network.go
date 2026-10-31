package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Localnet lists explicitly allowed TCP/UDP host:port destinations. Only names
// explicitly listed by the user are resolved; Qwen settings cannot add entries.
type Localnet []localEndpoint

type localEndpoint struct {
	Host string
	Port uint16
}

// ResolvedLocalnet is the startup DNS snapshot used by both the whitelist and
// /etc/hosts. Addresses contain only numeric endpoints; Hosts contains DNS names.
type ResolvedLocalnet struct {
	Addresses []netip.AddrPort
	Hosts     map[string][]netip.Addr
}

func (localnet *Localnet) Set(value string) error {
	var endpoints Localnet
	if value != "" {
		for entry := range strings.SplitSeq(value, ",") {
			host, portText, err := net.SplitHostPort(strings.TrimSpace(entry))
			if err != nil {
				return fmt.Errorf("parse localnet endpoint %q: %w", entry, err)
			}
			port, err := strconv.ParseUint(portText, 10, 16)
			if err != nil {
				return fmt.Errorf("parse localnet port %q: %w", portText, err)
			}
			if host == "" || port == 0 {
				return fmt.Errorf("localnet endpoint %q requires a hostname or IP address and a nonzero port", entry)
			}
			if address, err := netip.ParseAddr(host); err == nil {
				if address.Zone() != "" {
					return fmt.Errorf("localnet endpoint %q must not contain an IPv6 zone", entry)
				}
				host = address.Unmap().String()
			} else {
				// Validate DNS names before resolving them or writing them to hosts.
				name := strings.TrimSuffix(host, ".")
				if len(name) == 0 || len(name) > 253 {
					return fmt.Errorf("invalid localnet hostname %q", host)
				}
				for label := range strings.SplitSeq(name, ".") {
					if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
						return fmt.Errorf("invalid localnet hostname %q", host)
					}
					for _, character := range label {
						if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
							return fmt.Errorf("invalid localnet hostname %q", host)
						}
					}
				}
				host = strings.ToLower(host)
			}
			endpoints = append(endpoints, localEndpoint{Host: host, Port: uint16(port)})
		}
	}
	slices.SortFunc(endpoints, func(a, b localEndpoint) int {
		if comparison := strings.Compare(a.Host, b.Host); comparison != 0 {
			return comparison
		}
		return cmp.Compare(a.Port, b.Port)
	})
	*localnet = slices.Compact(endpoints)
	return nil
}

func (localnet Localnet) String() string {
	entries := make([]string, len(localnet))
	for i, endpoint := range localnet {
		entries[i] = net.JoinHostPort(endpoint.Host, strconv.Itoa(int(endpoint.Port)))
	}
	return strings.Join(entries, ",")
}

func (Localnet) Type() string { return "host:port,..." }

func (localnet Localnet) Resolve(ctx context.Context) (ResolvedLocalnet, error) {
	return localnet.resolve(ctx, net.DefaultResolver)
}

func (localnet Localnet) resolve(ctx context.Context, resolver *net.Resolver) (ResolvedLocalnet, error) {
	result := ResolvedLocalnet{Hosts: map[string][]netip.Addr{}}
	for _, endpoint := range localnet {
		if err := ctx.Err(); err != nil {
			return ResolvedLocalnet{}, err
		}
		var addresses []netip.Addr
		if address, err := netip.ParseAddr(endpoint.Host); err == nil {
			addresses = []netip.Addr{address.Unmap()}
		} else if cached, ok := result.Hosts[endpoint.Host]; ok {
			addresses = cached
		} else {
			var err error
			addresses, err = resolver.LookupNetIP(ctx, "ip", endpoint.Host)
			if err != nil {
				return ResolvedLocalnet{}, fmt.Errorf("LookupNetIP(%s): %w", endpoint.Host, err)
			}
			if len(addresses) == 0 {
				return ResolvedLocalnet{}, fmt.Errorf("localnet hostname %q resolved to no addresses", endpoint.Host)
			}
			for i, address := range addresses {
				addresses[i] = address.Unmap()
			}
			slices.SortFunc(addresses, netip.Addr.Compare)
			addresses = slices.Compact(addresses)
			result.Hosts[endpoint.Host] = addresses
		}
		for _, address := range addresses {
			result.Addresses = append(result.Addresses, netip.AddrPortFrom(address, endpoint.Port))
		}
	}
	slices.SortFunc(result.Addresses, netip.AddrPort.Compare)
	result.Addresses = slices.Compact(result.Addresses)
	return result, nil
}

func parsePrefix(value string) (netip.Prefix, error) {
	// iproute2 may print host routes without a prefix length (/32 or /128).
	if !strings.Contains(value, "/") {
		address, err := netip.ParseAddr(value)
		if err != nil || address.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("invalid IP address %q", value)
		}
		address = address.Unmap()
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("ParsePrefix(%q): %w", value, err)
	}
	if prefix.Addr().Is4In6() {
		if prefix.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("IPv4-mapped prefix %q must be /96 or longer", value)
		}
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	return prefix.Masked(), nil
}

type hostRoute struct {
	Scope       string `json:"scope"`
	Device      string `json:"dev"`
	Gateway     string `json:"gateway"`
	Type        string `json:"type"`
	Destination string `json:"dst"`
}

type hostInterface struct {
	Addresses []struct {
		Family string `json:"family"`
		Local  string `json:"local"`
	} `json:"addr_info"`
}

type HostNetwork struct {
	LinkRoutes []netip.Prefix
	Addresses  []netip.Addr
}

func discoverNetwork(ctx context.Context, ip string) (HostNetwork, error) {
	var network HostNetwork
	for _, family := range []string{"-4", "-6"} {
		var routes []hostRoute
		if err := readIPJSON(ctx, ip, &routes, family, "route", "show", "table", "all"); err != nil {
			return network, err
		}
		for _, route := range routes {
			// IPv6 connected routes often omit scope. A unicast route directly
			// through a device, without a gateway, identifies the connected prefix.
			if route.Destination == "" || route.Destination == "default" || route.Device == "" || route.Gateway != "" || route.Scope == "host" || (route.Type != "" && route.Type != "unicast") {
				continue
			}
			prefix, err := parsePrefix(route.Destination)
			if err != nil {
				return network, err
			}
			network.LinkRoutes = append(network.LinkRoutes, prefix)
		}
	}
	var interfaces []hostInterface
	if err := readIPJSON(ctx, ip, &interfaces, "address", "show"); err != nil {
		return network, err
	}
	for _, iface := range interfaces {
		for _, address := range iface.Addresses {
			if address.Family != "inet" && address.Family != "inet6" {
				continue
			}
			ip, err := netip.ParseAddr(address.Local)
			if err != nil {
				return network, fmt.Errorf("invalid host IP address %q", address.Local)
			}
			network.Addresses = append(network.Addresses, ip.Unmap())
		}
	}
	return network, nil
}

func readIPJSON(ctx context.Context, ip string, destination any, args ...string) error {
	cmd := exec.CommandContext(ctx, ip, append([]string{"-j"}, args...)...)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("inspect host network (%s): %w", strings.Join(args, " "), err)
	}
	if err := json.Unmarshal(output, destination); err != nil {
		return fmt.Errorf("decode ip output: %w", err)
	}
	return nil
}

// collapsePrefixes removes overlap and merges adjacent sibling networks, keeping
// destination filters compact without duplicate rules.
func collapsePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	ordered := slices.Clone(prefixes)
	slices.SortFunc(ordered, func(a, b netip.Prefix) int {
		if comparison := a.Addr().Compare(b.Addr()); comparison != 0 {
			return comparison
		}
		return cmp.Compare(a.Bits(), b.Bits())
	})
	var result []netip.Prefix
	for _, prefix := range ordered {
		prefix = prefix.Masked()
		if len(result) > 0 && result[len(result)-1].Contains(prefix.Addr()) {
			continue
		}
		result = append(result, prefix)
		for len(result) >= 2 {
			last := len(result) - 1
			a, b := result[last-1], result[last]
			if a.Bits() == 0 || a.Bits() != b.Bits() {
				break
			}
			parent := netip.PrefixFrom(a.Addr(), a.Bits()-1).Masked()
			if !parent.Contains(b.Addr()) {
				break
			}
			result = append(result[:last-1], parent)
		}
	}
	return result
}
