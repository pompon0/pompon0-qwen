package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestModelServerValidation(t *testing.T) {
	for _, value := range []string{"http://192.168.1.50:11434/v1", "https://8.8.8.8:65535/v1", "http://10.1.1.1:1", "http://[fd12:3456::50]:11434/v1", "https://[2606:4700:4700::1111]/v1", "http://[::ffff:192.168.1.50]:11434/v1"} {
		server, err := modelServerFromURL(value)
		endpoint, present := server.Get()
		if err != nil || !present || endpoint.Port() == 0 {
			t.Errorf("rejected %q: %v", value, err)
		}
	}
	for _, value := range []string{"localhost:11434", "ftp://192.168.1.50/v1", "/v1", "http://localhost:11434", "http://[::1]:11434", "http://127.0.0.1:1", "http://169.254.1.1:1", "http://224.0.0.1:1", "http://240.1.1.1:1", "http://0.0.0.0:1", "http://10.0.2.15:11434", "http://1.1.1.1:0", "http://1.1.1.1:65536", "http://1.1.1.1:+80", "http://1.1.1.1:８０", "http://1.1.1.1:", "http://[::]:11434", "http://[ff02::1]:11434", "http://[fe80::1]:11434", "http://[::ffff:127.0.0.1]:11434", "http://[2001:4860::1%25eth0]:11434", "http://[fd00:2::15]:11434"} {
		if _, err := modelServerFromURL(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	if server, err := modelServerFromURL(""); server.IsPresent() || err != nil {
		t.Fatal("empty exception must be disabled")
	}
	for endpoint, port := range map[string]uint16{"http://192.168.1.50/v1": 80, "https://192.168.1.50/v1": 443, "http://[fd12::50]/v1": 80, "https://[fd12::50]/v1": 443} {
		server, err := modelServerFromURL(endpoint)
		value, present := server.Get()
		if err != nil || !present || value.Port() != port {
			t.Errorf("default port for %q: %v, %v", endpoint, server, err)
		}
	}
	if server, err := modelServerFromURL("https://api.example.com/v1"); server.IsPresent() || err != nil {
		t.Fatal("public DNS endpoints should use ordinary filtered networking")
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
	server, err := modelServerFromURL("http://192.168.1.50:11434/v1")
	if err != nil || !server.IsPresent() {
		t.Fatal(err)
	}
	policy := newNetworkPolicy(HostNetwork{
		LinkRoutes: []netip.Prefix{netip.MustParsePrefix("11.2.0.0/16"), netip.MustParsePrefix("2001:4860:abcd::/64")},
		Addresses:  []netip.Addr{netip.MustParseAddr("9.8.7.6"), netip.MustParseAddr("2001:4860::8888")},
	}, []netip.Prefix{netip.MustParsePrefix("12.0.0.0/8"), netip.MustParsePrefix("2606:4700:1234::/64")}, server)
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
	server, err := modelServerFromURL("http://[fd12:3456::50]:11434/v1")
	if err != nil {
		t.Fatal(err)
	}
	rules := newNetworkPolicy(HostNetwork{}, nil, server).NFTables()
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
