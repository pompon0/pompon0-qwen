package main

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// NetworkPolicy describes the destinations permitted by the supervisor.
type NetworkPolicy struct {
	Blocked      []netip.Prefix
	ModelServers []netip.AddrPort
}

func newNetworkPolicy(host HostNetwork, extra []netip.Prefix, servers []netip.AddrPort) NetworkPolicy {
	var blocked []netip.Prefix
	for value := range strings.FieldsSeq(`0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8
		169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24
		192.88.99.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24
		203.0.113.0/24 224.0.0.0/4 240.0.0.0/4
		::/96 ::ffff:0:0/96 64:ff9b:1::/48 100::/64 100:0:0:1::/64
		2001::/23 2001:db8::/32 2002::/16 3fff::/20 5f00::/16
		fc00::/7 fe80::/10 fec0::/10 ff00::/8`) {
		blocked = append(blocked, netip.MustParsePrefix(value))
	}
	blocked = append(blocked, extra...)
	blocked = append(blocked, host.LinkRoutes...)
	for _, address := range host.Addresses {
		blocked = append(blocked, netip.PrefixFrom(address, address.BitLen()))
	}
	// The well-known NAT64 prefix embeds IPv4 in its final 32 bits. Apply the
	// same blocks there so IPv6 translation cannot bypass IPv4 restrictions.
	for _, prefix := range slices.Clone(blocked) {
		if prefix.Addr().Is4() {
			address := netip.MustParseAddr("64:ff9b::").As16()
			ipv4 := prefix.Addr().As4()
			copy(address[12:], ipv4[:])
			blocked = append(blocked, netip.PrefixFrom(netip.AddrFrom16(address), 96+prefix.Bits()))
		}
	}
	return NetworkPolicy{Blocked: collapsePrefixes(blocked), ModelServers: servers}
}

// Rules are installed only inside the managed Podman container, before Qwen.
// No host firewall is modified. The model exception precedes destination blocks.
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
	for _, model := range policy.ModelServers {
		family := "ip"
		if model.Addr().Is6() {
			family = "ip6"
		}
		exceptions = append(exceptions, fmt.Sprintf("%s daddr %s tcp dport %d accept", family, model.Addr(), model.Port()))
	}
	return fmt.Sprintf(`table inet qwen_egress {
 set blocked4 { type ipv4_addr; flags interval; %s }
 set blocked6 { type ipv6_addr; flags interval; %s }
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
