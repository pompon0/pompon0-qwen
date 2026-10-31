package main

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/netip"
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

func (localnet Localnet) Resolve(ctx context.Context) ([]netip.AddrPort, error) {
	return localnet.resolve(ctx, net.DefaultResolver)
}

func (localnet Localnet) resolve(ctx context.Context, resolver *net.Resolver) ([]netip.AddrPort, error) {
	var result []netip.AddrPort
	for _, endpoint := range localnet {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var addresses []netip.Addr
		if address, err := netip.ParseAddr(endpoint.Host); err == nil {
			addresses = []netip.Addr{address.Unmap()}
		} else {
			var err error
			addresses, err = resolver.LookupNetIP(ctx, "ip", endpoint.Host)
			if err != nil {
				return nil, fmt.Errorf("LookupNetIP(%s): %w", endpoint.Host, err)
			}
			if len(addresses) == 0 {
				return nil, fmt.Errorf("localnet hostname %q resolved to no addresses", endpoint.Host)
			}
			for i, address := range addresses {
				addresses[i] = address.Unmap()
			}
		}
		for _, address := range addresses {
			result = append(result, netip.AddrPortFrom(address, endpoint.Port))
		}
	}
	slices.SortFunc(result, netip.AddrPort.Compare)
	result = slices.Compact(result)
	return result, nil
}
