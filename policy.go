package main

import (
	"fmt"
	"net/netip"
	"strings"
)

// NetworkPolicy describes the destinations permitted by the supervisor.
type NetworkPolicy struct {
	Blocked  []netip.Prefix
	Localnet []netip.AddrPort
}

// The blacklist is fixed: publicly routable host and LAN addresses are treated
// like other public destinations, without inspecting host interfaces or routes.
// Initialized once, including NAT64 counterparts, and read-only thereafter.
var blockedPrefixes = func() []netip.Prefix {
	blocked := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/96"),
		netip.MustParsePrefix("::ffff:0:0/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("100:0:0:1::/64"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("3fff::/20"),
		netip.MustParsePrefix("5f00::/16"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("fec0::/10"),
		netip.MustParsePrefix("ff00::/8"),
	}
	// The well-known NAT64 prefix embeds IPv4 in its final 32 bits. Apply the
	// same blocks there so IPv6 translation cannot bypass IPv4 restrictions.
	for _, prefix := range blocked {
		if prefix.Addr().Is4() {
			address := netip.MustParseAddr("64:ff9b::").As16()
			ipv4 := prefix.Addr().As4()
			copy(address[12:], ipv4[:])
			blocked = append(blocked, netip.PrefixFrom(netip.AddrFrom16(address), 96+prefix.Bits()))
		}
	}
	return blocked
}()

func newNetworkPolicy(localnet []netip.AddrPort) NetworkPolicy {
	return NetworkPolicy{Blocked: blockedPrefixes, Localnet: localnet}
}

// Rules are installed only inside the managed Podman container, before Qwen.
// No host firewall is modified. Explicit localnet exceptions precede destination blocks.
func (policy NetworkPolicy) NFTables() string {
	var blocked4, blocked6 []string
	for _, prefix := range policy.Blocked {
		if prefix.Addr().Is4() {
			blocked4 = append(blocked4, prefix.String())
		} else {
			blocked6 = append(blocked6, prefix.String())
		}
	}
	elements := func(blocked []string) string {
		if len(blocked) == 0 {
			return ""
		}
		return "elements = { " + strings.Join(blocked, ", ") + " }"
	}
	var exceptions []string
	for _, endpoint := range policy.Localnet {
		family := "ip"
		if endpoint.Addr().Is6() {
			family = "ip6"
		}
		for _, protocol := range []string{"tcp", "udp"} {
			exceptions = append(exceptions, fmt.Sprintf("%s daddr %s %s dport %d accept", family, endpoint.Addr(), protocol, endpoint.Port()))
		}
	}
	return fmt.Sprintf(`table inet qwen_egress {
 set blocked4 { type ipv4_addr; flags interval; auto-merge; %s }
 set blocked6 { type ipv6_addr; flags interval; auto-merge; %s }
 chain output {
  type filter hook output priority 0; policy drop;
  oifname "lo" accept
  %s
  # IPv6 needs neighbor discovery, router discovery, and path-MTU errors.
  # These control messages do not grant TCP/UDP access to blocked destinations.
  ip6 hoplimit 255 icmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept
  ip daddr @blocked4 reject with icmpx type admin-prohibited
  ip6 daddr @blocked6 reject with icmpx type admin-prohibited
  meta nfproto { ipv4, ipv6 } accept
 }
 chain input {
  type filter hook input priority 0; policy drop;
  iifname "lo" accept
  ct state established,related accept
  icmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem } accept
  ip6 hoplimit 255 icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept
 }
 chain forward { type filter hook forward priority 0; policy drop; }
}
`, elements(blocked4), elements(blocked6), strings.Join(exceptions, "\n  "))
}
