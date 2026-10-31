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
	"time"

	"github.com/pompon0/pompon0-qwen/utils/scope"
)

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
	policy := newNetworkPolicy(HostNetwork{
		LinkRoutes: []netip.Prefix{netip.MustParsePrefix("11.2.0.0/16"), netip.MustParsePrefix("2001:4860:abcd::/64")},
		Addresses:  []netip.Addr{netip.MustParseAddr("9.8.7.6"), netip.MustParseAddr("2001:4860::8888")},
	}, []netip.AddrPort{netip.MustParseAddrPort("192.168.1.50:11434")})
	for _, value := range []string{"10.0.0.1", "127.0.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "198.18.0.1", "224.0.0.1", "11.2.3.4", "9.8.7.6", "::", "::1", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1", "2001:4860:abcd::1", "2001:4860::8888", "64:ff9b::a00:1", "64:ff9b::908:706"} {
		if !policyBlocks(policy, netip.MustParseAddr(value)) {
			t.Errorf("policy does not block %s", value)
		}
	}
	for _, address := range []string{"1.1.1.1", "2606:4700:4700::1111", "64:ff9b::101:101", "2001:4860::8889", "12.3.4.5", "2606:4700:1234::1"} {
		if policyBlocks(policy, netip.MustParseAddr(address)) {
			t.Fatalf("public internet blocked: %s", address)
		}
	}
	rules := policy.NFTables()
	if strings.Index(rules, "ip daddr 192.168.1.50 tcp dport 11434 accept") < 0 || strings.Index(rules, "ip daddr 192.168.1.50 tcp dport 11434 accept") > strings.Index(rules, "ip daddr @blocked4") {
		t.Fatal("explicit localnet exception must precede destination blocks")
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

func TestIPv6LocalnetException(t *testing.T) {
	rules := newNetworkPolicy(HostNetwork{}, []netip.AddrPort{netip.MustParseAddrPort("[fd12:3456::50]:11434")}).NFTables()
	exception := strings.Index(rules, "ip6 daddr fd12:3456::50 tcp dport 11434 accept")
	if exception < 0 || exception > strings.Index(rules, "ip6 daddr @blocked6") {
		t.Fatal("explicit IPv6 localnet exception must precede destination blocks")
	}
}

func TestLocalnetExceptionsAreExplicit(t *testing.T) {
	if rules := newNetworkPolicy(HostNetwork{}, nil).NFTables(); strings.Contains(rules, "tcp dport") || strings.Contains(rules, "udp dport") {
		t.Fatal("default policy must not grant local TCP/UDP exceptions")
	}
	allowed := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.50:11434"), netip.MustParseAddrPort("[fd12::50]:8000")}
	rules := newNetworkPolicy(HostNetwork{}, allowed).NFTables()
	for _, rule := range []string{"ip daddr 192.168.1.50 tcp dport 11434 accept", "ip daddr 192.168.1.50 udp dport 11434 accept", "ip6 daddr fd12::50 tcp dport 8000 accept", "ip6 daddr fd12::50 udp dport 8000 accept"} {
		index := strings.Index(rules, rule)
		if index < 0 || index > strings.Index(rules, "ip daddr @blocked4") {
			t.Fatalf("missing localnet exception before blocks: %s", rule)
		}
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
func TestLocalnetDNSResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
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
				clear(response[6:12])
				answers := uint16(0)
				if name == "local.test." {
					for _, value := range []string{"192.168.1.50", "192.168.1.50", "192.168.1.60", "fd12::50"} {
						address := netip.MustParseAddr(value)
						if (kind == 1 && address.Is4()) || (kind == 28 && address.Is6()) {
							response = append(response, 0xc0, 0x0c)
							response = binary.BigEndian.AppendUint16(response, kind)
							response = binary.BigEndian.AppendUint16(response, 1)
							response = binary.BigEndian.AppendUint32(response, 60)
							response = binary.BigEndian.AppendUint16(response, uint16(len(address.AsSlice())))
							response = append(response, address.AsSlice()...)
							answers++
						}
					}
				} else {
					binary.BigEndian.PutUint16(response[2:4], 0x8183)
				}
				binary.BigEndian.PutUint16(response[6:8], answers)
				if _, err := connection.WriteTo(response, peer); err != nil {
					return err
				}
			}
		})
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, "udp4", connection.LocalAddr().String())
		}}
		var localnet Localnet
		if err := localnet.Set("local.test:8000,local.test:9000,192.168.1.50:8000"); err != nil {
			return err
		}
		resolved, err := localnet.resolve(ctx, resolver)
		if err != nil {
			return err
		}
		wantAddresses := []netip.Addr{netip.MustParseAddr("192.168.1.50"), netip.MustParseAddr("192.168.1.60"), netip.MustParseAddr("fd12::50")}
		if !slices.Equal(resolved.Hosts["local.test"], wantAddresses) || len(resolved.Addresses) != 6 {
			return fmt.Errorf("unexpected DNS snapshot: %+v", resolved)
		}
		rules := newNetworkPolicy(HostNetwork{}, resolved.Addresses).NFTables()
		for _, address := range wantAddresses {
			for _, port := range []uint16{8000, 9000} {
				if !slices.Contains(resolved.Addresses, netip.AddrPortFrom(address, port)) || !strings.Contains(rules, fmt.Sprintf("daddr %s tcp dport %d accept", address, port)) || !strings.Contains(rules, fmt.Sprintf("daddr %s udp dport %d accept", address, port)) {
					return fmt.Errorf("missing resolved destination %s:%d", address, port)
				}
			}
		}
		runDir := t.TempDir()
		if err := writeRunFiles(runDir, "/host/home", ModeQwen, resolved); err != nil {
			return err
		}
		hosts, err := os.ReadFile(filepath.Join(runDir, "rootfs/etc/hosts"))
		if err != nil {
			return err
		}
		for _, address := range wantAddresses {
			if !strings.Contains(string(hosts), address.String()+" local.test\n") {
				return fmt.Errorf("resolved hostname missing from hosts: %s", address)
			}
		}
		if err := localnet.Set("local.test:8000,missing.test:9000"); err != nil {
			return err
		}
		failed, err := localnet.resolve(ctx, resolver)
		if err == nil || len(failed.Addresses) != 0 || len(failed.Hosts) != 0 {
			return fmt.Errorf("failed DNS lookup must discard the entire whitelist: %+v, %v", failed, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalnetResolutionCancellation(t *testing.T) {
	var localnet Localnet
	if err := localnet.Set("local.test:8000"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := localnet.Resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
