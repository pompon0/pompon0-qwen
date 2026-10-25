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

type ModelServer struct {
	Address netip.Addr
	Port    uint16
}

// modelServerFromURL never resolves hostnames on the host. Public DNS names
// continue to use normal filtered egress; LAN exceptions require a literal IPv4.
// None means the endpoint needs no explicit model-server exception.
func modelServerFromURL(value string) (utils.Option[ModelServer], error) {
	if value == "" {
		return utils.None[ModelServer](), nil
	}
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return utils.None[ModelServer](), fmt.Errorf("model baseUrl must be an absolute HTTP or HTTPS URL")
	}
	if strings.Contains(endpoint.Host, "$") {
		return utils.None[ModelServer](), fmt.Errorf("unresolved variable in model baseUrl; use a literal endpoint or define it in settings.env")
	}
	portText := endpoint.Port()
	if portText == "" {
		if strings.HasSuffix(endpoint.Host, ":") {
			return utils.None[ModelServer](), fmt.Errorf("model baseUrl has an empty TCP port")
		}
		portText = "80"
		if endpoint.Scheme == "https" {
			portText = "443"
		}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return utils.None[ModelServer](), fmt.Errorf("model baseUrl has an invalid TCP port")
	}
	ip, err := netip.ParseAddr(endpoint.Hostname())
	if err != nil {
		if strings.EqualFold(endpoint.Hostname(), "localhost") || strings.HasSuffix(strings.ToLower(endpoint.Hostname()), ".localhost") {
			return utils.None[ModelServer](), fmt.Errorf("model baseUrl must use a LAN/public IPv4 address instead of localhost")
		}
		return utils.None[ModelServer](), nil
	}
	if !ip.Is4() {
		return utils.None[ModelServer](), fmt.Errorf("IPv6 model endpoints are not supported; use a LAN/public IPv4 address")
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || netip.MustParsePrefix("240.0.0.0/4").Contains(ip) {
		return utils.None[ModelServer](), fmt.Errorf("model baseUrl must use a LAN/public IPv4 address")
	}
	if netip.MustParsePrefix(networkSubnet).Contains(ip) {
		return utils.None[ModelServer](), fmt.Errorf("model baseUrl overlaps the sandbox virtual subnet 10.0.2.0/24")
	}
	return utils.Some(ModelServer{Address: ip, Port: uint16(port)}), nil
}

func parseIPv4Prefix(value string) (netip.Prefix, error) {
	// iproute2 may print a host route without /32.
	if !strings.Contains(value, "/") {
		value += "/32"
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid IPv4 prefix %q", value)
	}
	return prefix.Masked(), nil
}

type hostRoute struct {
	Scope       string `json:"scope"`
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
	var routes []hostRoute
	if err := readIPJSON(ctx, ip, &routes, "route", "show", "table", "all"); err != nil {
		return network, err
	}
	for _, route := range routes {
		if route.Scope != "link" || route.Destination == "" || route.Destination == "default" {
			continue
		}
		prefix, err := parseIPv4Prefix(route.Destination)
		if err != nil {
			return network, err
		}
		network.LinkRoutes = append(network.LinkRoutes, prefix)
	}
	var interfaces []hostInterface
	if err := readIPJSON(ctx, ip, &interfaces, "address", "show"); err != nil {
		return network, err
	}
	for _, iface := range interfaces {
		for _, address := range iface.Addresses {
			if address.Family != "inet" {
				continue
			}
			ip, err := netip.ParseAddr(address.Local)
			if err != nil || !ip.Is4() {
				return network, fmt.Errorf("invalid host IPv4 address %q", address.Local)
			}
			network.Addresses = append(network.Addresses, ip)
		}
	}
	return network, nil
}

func readIPJSON(ctx context.Context, ip string, destination any, args ...string) error {
	cmd := exec.CommandContext(ctx, ip, append([]string{"-j", "-4"}, args...)...)
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
