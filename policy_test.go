package main

import (
	"strings"
	"testing"
)

func TestFirewallRejectsInvalidPolicy(t *testing.T) {
	for _, input := range []string{
		``,
		`{`,
		`{"Unknown":true}`,
		`{"Blocked":[""]}`,
		`{"Blocked":["invalid"]}`,
		`{"Localnet":[""]}`,
		`{"Localnet":["192.168.1.50:0"]}`,
		`{"Localnet":["[fe80::1%eth0]:8000"]}`,
	} {
		if err := installFirewall(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid firewall input %q", input)
		}
	}
}
