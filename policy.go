package main

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/pompon0/pompon0-qwen/utils"
)

// NetworkPolicy describes the destinations permitted by the supervisor.
type NetworkPolicy struct {
	Blocked     []netip.Prefix
	ModelServer utils.Option[netip.AddrPort]
}

func newNetworkPolicy(host HostNetwork, extra []netip.Prefix, server utils.Option[netip.AddrPort]) NetworkPolicy {
	var blocked []netip.Prefix
	for value := range strings.FieldsSeq(`0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8
		169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24
		192.88.99.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24
		203.0.113.0/24 224.0.0.0/4 240.0.0.0/4`) {
		blocked = append(blocked, netip.MustParsePrefix(value))
	}
	blocked = append(blocked, extra...)
	blocked = append(blocked, host.LinkRoutes...)
	for _, address := range host.Addresses {
		blocked = append(blocked, netip.PrefixFrom(address, 32))
	}
	return NetworkPolicy{Blocked: collapsePrefixes(blocked), ModelServer: server}
}

// Rules are installed only inside the managed Podman container, before Qwen.
// No host firewall is modified. The model exception precedes destination blocks.
func (policy NetworkPolicy) NFTables() string {
	blocked := make([]string, len(policy.Blocked))
	for i, prefix := range policy.Blocked {
		blocked[i] = prefix.String()
	}
	elements := ""
	if len(blocked) > 0 {
		elements = "elements = { " + strings.Join(blocked, ", ") + " }"
	}
	exception := ""
	if model, present := policy.ModelServer.Get(); present {
		exception = fmt.Sprintf("ip daddr %s tcp dport %d accept", model.Addr(), model.Port())
	}
	return fmt.Sprintf(`table inet qwen_egress {
 set blocked4 { type ipv4_addr; flags interval; %s }
 chain output {
  type filter hook output priority 0; policy drop;
  oifname "lo" accept
  %s
  ip daddr @blocked4 reject with icmpx type admin-prohibited
  meta nfproto ipv4 accept
 }
 chain input {
  type filter hook input priority 0; policy drop;
  iifname "lo" accept
  ct state established,related accept
 }
 chain forward { type filter hook forward priority 0; policy drop; }
}
`, elements, exception)
}
