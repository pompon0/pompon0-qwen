package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/pompon0/pompon0-qwen/utils"
)

var networkSubnets = [...]netip.Prefix{
	netip.MustParsePrefix("10.0.2.0/24"),
	netip.MustParsePrefix("fd00:2::/64"),
}

// modelServerFromURL never resolves hostnames on the host. Public DNS names
// continue to use normal filtered egress; LAN exceptions require a literal IP address.
// None means the endpoint needs no explicit model-server exception.
func modelServerFromURL(value string) (utils.Option[netip.AddrPort], error) {
	if value == "" {
		return utils.None[netip.AddrPort](), nil
	}
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return utils.None[netip.AddrPort](), fmt.Errorf("model baseUrl must be an absolute HTTP or HTTPS URL")
	}
	if strings.Contains(endpoint.Host, "$") {
		return utils.None[netip.AddrPort](), fmt.Errorf("unresolved variable in model baseUrl; use a literal endpoint or define it in settings.env")
	}
	portText := endpoint.Port()
	if portText == "" {
		if strings.HasSuffix(endpoint.Host, ":") {
			return utils.None[netip.AddrPort](), fmt.Errorf("model baseUrl has an empty TCP port")
		}
		portText = "80"
		if endpoint.Scheme == "https" {
			portText = "443"
		}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return utils.None[netip.AddrPort](), fmt.Errorf("model baseUrl has an invalid TCP port")
	}
	ip, err := netip.ParseAddr(endpoint.Hostname())
	if err != nil {
		if strings.EqualFold(endpoint.Hostname(), "localhost") || strings.HasSuffix(strings.ToLower(endpoint.Hostname()), ".localhost") {
			return utils.None[netip.AddrPort](), fmt.Errorf("model baseUrl must use a LAN/public IP address instead of localhost")
		}
		return utils.None[netip.AddrPort](), nil
	}
	ip = ip.Unmap()
	if ip.Zone() != "" {
		return utils.None[netip.AddrPort](), fmt.Errorf("model baseUrl must not contain an IPv6 zone")
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || netip.MustParsePrefix("240.0.0.0/4").Contains(ip) {
		return utils.None[netip.AddrPort](), fmt.Errorf("model baseUrl must use a LAN/public IP address")
	}
	for _, subnet := range networkSubnets {
		if subnet.Contains(ip) {
			return utils.None[netip.AddrPort](), fmt.Errorf("model baseUrl overlaps the sandbox virtual subnet %s", subnet)
		}
	}
	return utils.Some(netip.AddrPortFrom(ip, uint16(port))), nil
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
