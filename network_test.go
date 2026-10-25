package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pompon0/pompon0-qwen/utils/scope"
)

func TestModelEndpointResolution(t *testing.T) {
	for _, test := range []struct{ url, want string }{
		{"http://192.168.1.50:11434/v1", "192.168.1.50:11434"},
		{"https://8.8.8.8:65535/v1", "8.8.8.8:65535"},
		{"http://[fd12:3456::50]:11434/v1", "[fd12:3456::50]:11434"},
		{"http://[::ffff:192.168.1.50]:11434/v1", "192.168.1.50:11434"},
		{"http://192.168.1.50/v1", "192.168.1.50:80"},
		{"https://192.168.1.50/v1", "192.168.1.50:443"},
		{"https://[fd12::50]/v1", "[fd12::50]:443"},
		{"ftp://192.168.1.50/v1", "192.168.1.50:21"},
		{"custom://192.168.1.50:11434/v1", "192.168.1.50:11434"},
		{"http://127.0.0.1:11434", "127.0.0.1:11434"},
		{"http://[::1]:11434", "[::1]:11434"},
	} {
		endpoint, err := modelEndpointFromURL(t.Context(), test.url)
		if err != nil || !slices.Equal(endpoint.Addresses, []netip.AddrPort{netip.MustParseAddrPort(test.want)}) {
			t.Errorf("resolve %q: %+v, %v", test.url, endpoint, err)
		}
	}
	for _, value := range []string{"localhost:11434", "/v1", "custom://192.168.1.50/v1", "http://1.1.1.1:65536", "http://1.1.1.1:+80", "http://1.1.1.1:８０", "http://$MODEL_HOST:11434"} {
		if _, err := modelEndpointFromURL(t.Context(), value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	if endpoint, err := modelEndpointFromURL(t.Context(), ""); len(endpoint.Addresses) != 0 || err != nil {
		t.Fatal("missing endpoint must not grant an exception", endpoint, err)
	}
}

func TestPrefixParsing(t *testing.T) {
	for value, want := range map[string]string{"10.1.2.3/8": "10.0.0.0/8", "8.8.8.8": "8.8.8.8/32", "0.0.0.0/0": "0.0.0.0/0", "fd12::1234/64": "fd12::/64", "2001:4860::8888": "2001:4860::8888/128", "::/0": "::/0", "::ffff:192.168.1.1/120": "192.168.1.0/24"} {
		got, err := parsePrefix(value)
		if err != nil || got.String() != want {
			t.Errorf("%s: got %s, %v", value, got, err)
		}
	}
	for _, value := range []string{"::/129", "::ffff:1.1.1.1/80", "1.1.1.1/33", "garbage"} {
		if _, err := parsePrefix(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestCollapsePrefixes(t *testing.T) {
	input := []netip.Prefix{}
	for _, value := range []string{"10.0.3.0/24", "10.0.1.0/24", "10.0.0.0/24", "10.0.2.0/24", "10.0.1.128/25", "10.0.0.0/24", "8.8.8.8/32", "fd12::/65", "fd12:0:0:0:8000::/65"} {
		input = append(input, netip.MustParsePrefix(value))
	}
	got := collapsePrefixes(input)
	want := []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32"), netip.MustParsePrefix("10.0.0.0/22"), netip.MustParsePrefix("fd12::/64")}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v; want %v", got, want)
	}
}

func TestPolicyCoversHostAndRoutes(t *testing.T) {
	server, err := modelEndpointFromURL(t.Context(), "http://192.168.1.50:11434/v1")
	if err != nil || len(server.Addresses) == 0 {
		t.Fatal(err)
	}
	policy := newNetworkPolicy(HostNetwork{
		LinkRoutes: []netip.Prefix{netip.MustParsePrefix("11.2.0.0/16"), netip.MustParsePrefix("2001:4860:abcd::/64")},
		Addresses:  []netip.Addr{netip.MustParseAddr("9.8.7.6"), netip.MustParseAddr("2001:4860::8888")},
	}, []netip.Prefix{netip.MustParsePrefix("12.0.0.0/8"), netip.MustParsePrefix("2606:4700:1234::/64")}, server.Addresses)
	for _, value := range []string{"10.0.0.1", "127.0.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "198.18.0.1", "224.0.0.1", "11.2.3.4", "9.8.7.6", "12.3.4.5", "::", "::1", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1", "2001:4860:abcd::1", "2001:4860::8888", "2606:4700:1234::1", "64:ff9b::a00:1", "64:ff9b::908:706"} {
		if !policyBlocks(policy, netip.MustParseAddr(value)) {
			t.Errorf("policy does not block %s", value)
		}
	}
	for _, address := range []string{"1.1.1.1", "2606:4700:4700::1111", "64:ff9b::101:101", "2001:4860::8889"} {
		if policyBlocks(policy, netip.MustParseAddr(address)) {
			t.Fatalf("public internet blocked: %s", address)
		}
	}
	rules := policy.NFTables()
	if strings.Index(rules, "ip daddr 192.168.1.50 tcp dport 11434 accept") < 0 || strings.Index(rules, "ip daddr 192.168.1.50 tcp dport 11434 accept") > strings.Index(rules, "ip daddr @blocked4") {
		t.Fatal("exact model exception must precede destination blocks")
	}

}

func policyBlocks(policy NetworkPolicy, address netip.Addr) bool {
	for _, prefix := range policy.Blocked {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func TestIPv6ModelException(t *testing.T) {
	server, err := modelEndpointFromURL(t.Context(), "http://[fd12:3456::50]:11434/v1")
	if err != nil {
		t.Fatal(err)
	}
	rules := newNetworkPolicy(HostNetwork{}, nil, server.Addresses).NFTables()
	exception := strings.Index(rules, "ip6 daddr fd12:3456::50 tcp dport 11434 accept")
	if exception < 0 || exception > strings.Index(rules, "ip6 daddr @blocked6") {
		t.Fatal("exact IPv6 model exception must precede destination blocks")
	}
}

func TestDiscoverNetworkBothFamilies(t *testing.T) {
	ip := filepath.Join(t.TempDir(), "ip")
	const script = `#!/bin/sh
case "$*" in
 '-j -4 route show table all') echo '[{"dst":"11.2.0.0/16","dev":"eth0","scope":"link"},{"dst":"default","gateway":"11.2.0.1","dev":"eth0"}]' ;;
 '-j -6 route show table all') echo '[{"dst":"2001:4860:abcd::/64","dev":"eth0"},{"dst":"2001:4860:1234::/64","gateway":"fe80::1","dev":"eth0"},{"dst":"2001:4860:abcd::1","dev":"eth0","type":"local"}]' ;;
 '-j address show') echo '[{"addr_info":[{"family":"inet","local":"11.2.0.2"},{"family":"inet6","local":"2001:4860:abcd::2"}]}]' ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(ip, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	network, err := discoverNetwork(t.Context(), ip)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(network.LinkRoutes, []netip.Prefix{netip.MustParsePrefix("11.2.0.0/16"), netip.MustParsePrefix("2001:4860:abcd::/64")}) {
		t.Fatalf("connected routes: %v", network.LinkRoutes)
	}
	if !slices.Equal(network.Addresses, []netip.Addr{netip.MustParseAddr("11.2.0.2"), netip.MustParseAddr("2001:4860:abcd::2")}) {
		t.Fatalf("host addresses: %v", network.Addresses)
	}
}

func TestModelEndpointDNS(t *testing.T) {
	// Serve real DNS replies locally: no external nameserver or production hook.
	err := scope.Run(t.Context(), func(ctx context.Context, s scope.Scope) error {
		connection, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer connection.Close()
		s.SpawnBg(func() error {
			buffer := make([]byte, 4096)
			for {
				n, peer, err := connection.ReadFrom(buffer)
				if errors.Is(err, net.ErrClosed) {
					return nil
				}
				if err != nil {
					return err
				}
				query := buffer[:n]
				name, offset := "", 12
				for query[offset] != 0 {
					length := int(query[offset])
					name += string(query[offset+1:offset+1+length]) + "."
					offset += 1 + length
				}
				offset++
				kind := binary.BigEndian.Uint16(query[offset:])
				response := append([]byte{}, query[:offset+4]...)
				binary.BigEndian.PutUint16(response[2:4], 0x8180)
				clear(response[6:12]) // No answers, authorities, or additional records yet.
				answers := uint16(0)
				if name == "model.test." {
					for _, text := range []string{"192.168.1.50", "192.168.1.50", "192.168.1.60", "fd12::50"} {
						address := netip.MustParseAddr(text)
						if (kind == 1 && address.Is4()) || (kind == 28 && address.Is6()) {
							response = append(response, 0xc0, 0x0c) // Name points to the question.
							response = binary.BigEndian.AppendUint16(response, kind)
							response = binary.BigEndian.AppendUint16(response, 1) // IN class.
							response = binary.BigEndian.AppendUint32(response, 60)
							response = binary.BigEndian.AppendUint16(response, uint16(len(address.AsSlice())))
							response = append(response, address.AsSlice()...)
							answers++
						}
					}
				} else {
					binary.BigEndian.PutUint16(response[2:4], 0x8183) // NXDOMAIN.
				}
				binary.BigEndian.PutUint16(response[6:8], answers)
				if _, err := connection.WriteTo(response, peer); err != nil {
					return err
				}
			}
		})
		original := net.DefaultResolver
		net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, "udp4", connection.LocalAddr().String())
		}}
		defer func() { net.DefaultResolver = original }()
		endpoint, err := modelEndpointFromURL(ctx, "https://model.test/v1")
		if err != nil {
			return err
		}
		want := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.50:443"), netip.MustParseAddrPort("192.168.1.60:443"), netip.MustParseAddrPort("[fd12::50]:443")}
		if endpoint.Hostname != "model.test" || !slices.Equal(endpoint.Addresses, want) {
			return fmt.Errorf("resolved endpoint %+v, want %v", endpoint, want)
		}
		rules := newNetworkPolicy(HostNetwork{}, nil, endpoint.Addresses).NFTables()
		for _, rule := range []string{"ip daddr 192.168.1.50 tcp dport 443 accept", "ip daddr 192.168.1.60 tcp dport 443 accept", "ip6 daddr fd12::50 tcp dport 443 accept"} {
			if !strings.Contains(rules, rule) {
				return fmt.Errorf("missing resolved address rule %s", rule)
			}
		}
		if _, err := modelEndpointFromURL(ctx, "https://missing.invalid/v1"); err == nil {
			return fmt.Errorf("unresolved endpoint did not fail startup")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
