package main

import (
	"net/netip"
	"strings"

	"github.com/pompon0/pompon0-qwen/utils"
)

// NetworkPolicy describes the destinations permitted by the supervisor.
type NetworkPolicy struct {
	Blocked     []netip.Prefix
	ModelServer utils.Option[ModelServer]
}

func newNetworkPolicy(host HostNetwork, extra []netip.Prefix, server utils.Option[ModelServer]) NetworkPolicy {
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

func (policy NetworkPolicy) blocks(address netip.Addr) bool {
	for _, prefix := range policy.Blocked {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (policy NetworkPolicy) allowsTCP(address netip.Addr, port uint16) bool {
	if model, present := policy.ModelServer.Get(); present && address == model.Address && port == model.Port {
		return true
	}
	return address.Is4() && !policy.blocks(address)
}
