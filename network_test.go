package main

import (
	"net/netip"
	"slices"
	"testing"
)

func TestModelServerValidation(t *testing.T) {
	for _, value := range []string{"http://192.168.1.50:11434/v1", "https://8.8.8.8:65535/v1", "http://10.1.1.1:1"} {
		server, err := modelServerFromURL(value)
		endpoint, present := server.Get()
		if err != nil || !present || endpoint.Port == 0 {
			t.Errorf("rejected %q: %v", value, err)
		}
	}
	for _, value := range []string{"localhost:11434", "ftp://192.168.1.50/v1", "/v1", "http://localhost:11434", "http://[::1]:11434", "http://127.0.0.1:1", "http://169.254.1.1:1", "http://224.0.0.1:1", "http://240.1.1.1:1", "http://0.0.0.0:1", "http://10.0.2.15:11434", "http://1.1.1.1:0", "http://1.1.1.1:65536", "http://1.1.1.1:+80", "http://1.1.1.1:８０", "http://1.1.1.1:"} {
		if _, err := modelServerFromURL(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	if server, err := modelServerFromURL(""); server.IsPresent() || err != nil {
		t.Fatal("empty exception must be disabled")
	}
	for endpoint, port := range map[string]uint16{"http://192.168.1.50/v1": 80, "https://192.168.1.50/v1": 443} {
		server, err := modelServerFromURL(endpoint)
		value, present := server.Get()
		if err != nil || !present || value.Port != port {
			t.Errorf("default port for %q: %v, %v", endpoint, server, err)
		}
	}
	if server, err := modelServerFromURL("https://api.example.com/v1"); server.IsPresent() || err != nil {
		t.Fatal("public DNS endpoints should use ordinary filtered networking")
	}
}

func TestIPv4PrefixParsing(t *testing.T) {
	for value, want := range map[string]string{"10.1.2.3/8": "10.0.0.0/8", "8.8.8.8": "8.8.8.8/32", "0.0.0.0/0": "0.0.0.0/0"} {
		got, err := parseIPv4Prefix(value)
		if err != nil || got.String() != want {
			t.Errorf("%s: got %s, %v", value, got, err)
		}
	}
	for _, value := range []string{"::/0", "::ffff:1.1.1.1/128", "1.1.1.1/33", "garbage"} {
		if _, err := parseIPv4Prefix(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestCollapsePrefixes(t *testing.T) {
	input := []netip.Prefix{}
	for _, value := range []string{"10.0.3.0/24", "10.0.1.0/24", "10.0.0.0/24", "10.0.2.0/24", "10.0.1.128/25", "10.0.0.0/24", "8.8.8.8/32"} {
		input = append(input, netip.MustParsePrefix(value))
	}
	got := collapsePrefixes(input)
	want := []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32"), netip.MustParsePrefix("10.0.0.0/22")}
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
		LinkRoutes: []netip.Prefix{netip.MustParsePrefix("11.2.0.0/16")},
		Addresses:  []netip.Addr{netip.MustParseAddr("9.8.7.6")},
	}, []netip.Prefix{netip.MustParsePrefix("12.0.0.0/8")}, server)
	for _, value := range []string{"10.0.0.1", "127.0.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "198.18.0.1", "224.0.0.1", "11.2.3.4", "9.8.7.6", "12.3.4.5"} {
		if !policyBlocks(policy, netip.MustParseAddr(value)) {
			t.Errorf("policy does not block %s", value)
		}
	}
	if policyBlocks(policy, netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public internet blocked")
	}
	if !policy.allowsTCP(netip.MustParseAddr("192.168.1.50"), 11434) || policy.allowsTCP(netip.MustParseAddr("192.168.1.50"), 8000) {
		t.Fatal("model exception must allow only its selected TCP port")
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
