package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
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
func (policy NetworkPolicy) Install() error {
	connection, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables.New(): %w", err)
	}
	table := connection.AddTable(&nftables.Table{Name: "qwen_egress", Family: nftables.TableFamilyINet})
	for _, chain := range []struct {
		name  string
		hook  nftables.ChainHook
		rules [][]expr.Any
	}{
		{"output", *nftables.ChainHookOutput, policy.outputRules()},
		{"input", *nftables.ChainHookInput, inputRules()},
		{"forward", *nftables.ChainHookForward, nil},
	} {
		drop := nftables.ChainPolicyDrop
		installed := connection.AddChain(&nftables.Chain{
			Name: chain.name, Table: table, Type: nftables.ChainTypeFilter,
			Hooknum: &chain.hook, Priority: nftables.ChainPriorityFilter, Policy: &drop,
		})
		for _, expressions := range chain.rules {
			connection.AddRule(&nftables.Rule{Table: table, Chain: installed, Exprs: expressions})
		}
	}
	// Commit the complete policy atomically. The short-lived connection opened
	// by Flush is closed by the library before it returns.
	if err := connection.Flush(); err != nil {
		return fmt.Errorf("nftables.Flush(): %w", err)
	}
	return nil
}

// installFirewall runs as the trusted setup process in the container namespace.
// It reads only the supervisor's policy, without discovering host configuration.
func installFirewall(input io.Reader) error {
	var policy NetworkPolicy
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return fmt.Errorf("decode firewall policy: %w", err)
	}
	for _, prefix := range policy.Blocked {
		if !prefix.IsValid() || prefix.Addr().Zone() != "" {
			return fmt.Errorf("invalid blocked prefix %s", prefix)
		}
	}
	for _, endpoint := range policy.Localnet {
		if !endpoint.IsValid() || endpoint.Port() == 0 || endpoint.Addr().Zone() != "" {
			return fmt.Errorf("invalid localnet endpoint %s", endpoint)
		}
	}
	return policy.Install()
}

func (policy NetworkPolicy) outputRules() [][]expr.Any {
	rules := [][]expr.Any{accept(metaEquals(expr.MetaKeyOIFNAME, []byte("lo\x00"))...)}
	for _, endpoint := range policy.Localnet {
		for _, protocol := range []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
			expressions := destinationMatches(netip.PrefixFrom(endpoint.Addr(), endpoint.Addr().BitLen()))
			expressions = append(expressions, metaEquals(expr.MetaKeyL4PROTO, []byte{protocol})...)
			expressions = append(expressions, payloadEquals(expr.PayloadBaseTransportHeader, 2, binaryutil.BigEndian.PutUint16(endpoint.Port()))...)
			rules = append(rules, accept(expressions...))
		}
	}
	// IPv6 needs neighbor discovery, router discovery, and path-MTU errors.
	// These control messages do not grant TCP/UDP access to blocked destinations.
	for _, kind := range []ipv6.ICMPType{ipv6.ICMPTypeRouterSolicitation, ipv6.ICMPTypeNeighborSolicitation, ipv6.ICMPTypeNeighborAdvertisement} {
		rules = append(rules, icmpv6Rule(kind, true))
	}
	// Separate prefix rules accept overlaps without userspace interval merging.
	for _, prefix := range policy.Blocked {
		rules = append(rules, append(destinationMatches(prefix), &expr.Reject{Type: unix.NFT_REJECT_ICMPX_UNREACH, Code: unix.NFT_REJECT_ICMPX_ADMIN_PROHIBITED}))
	}
	for _, family := range []byte{unix.NFPROTO_IPV4, unix.NFPROTO_IPV6} {
		rules = append(rules, accept(metaEquals(expr.MetaKeyNFPROTO, []byte{family})...))
	}
	return rules
}

func inputRules() [][]expr.Any {
	rules := [][]expr.Any{
		accept(metaEquals(expr.MetaKeyIIFNAME, []byte("lo\x00"))...),
		{
			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: make([]byte, 4)},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		},
	}
	for _, kind := range []ipv6.ICMPType{ipv6.ICMPTypeDestinationUnreachable, ipv6.ICMPTypePacketTooBig, ipv6.ICMPTypeTimeExceeded, ipv6.ICMPTypeParameterProblem} {
		rules = append(rules, icmpv6Rule(kind, false))
	}
	for _, kind := range []ipv6.ICMPType{ipv6.ICMPTypeRouterAdvertisement, ipv6.ICMPTypeNeighborSolicitation, ipv6.ICMPTypeNeighborAdvertisement} {
		rules = append(rules, icmpv6Rule(kind, true))
	}
	return rules
}

func accept(expressions ...expr.Any) []expr.Any {
	return append(expressions, &expr.Verdict{Kind: expr.VerdictAccept})
}

func metaEquals(key expr.MetaKey, data []byte) []expr.Any {
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: data}}
}

func payloadEquals(base expr.PayloadBase, offset uint32, data []byte) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: base, Offset: offset, Len: uint32(len(data))},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: data},
	}
}

func destinationMatches(prefix netip.Prefix) []expr.Any {
	address := prefix.Masked().Addr()
	family, offset := byte(unix.NFPROTO_IPV6), uint32(24)
	if address.Is4() {
		family, offset = unix.NFPROTO_IPV4, 16
	}
	data := address.AsSlice()
	expressions := metaEquals(expr.MetaKeyNFPROTO, []byte{family})
	expressions = append(expressions, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: uint32(len(data))})
	if prefix.Bits() != address.BitLen() {
		expressions = append(expressions, &expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: uint32(len(data)),
			Mask: net.CIDRMask(prefix.Bits(), address.BitLen()), Xor: make([]byte, len(data))})
	}
	return append(expressions, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: data})
}

func icmpv6Rule(kind ipv6.ICMPType, requireLinkHopLimit bool) []expr.Any {
	expressions := metaEquals(expr.MetaKeyNFPROTO, []byte{unix.NFPROTO_IPV6})
	expressions = append(expressions, metaEquals(expr.MetaKeyL4PROTO, []byte{unix.IPPROTO_ICMPV6})...)
	if requireLinkHopLimit {
		expressions = append(expressions, payloadEquals(expr.PayloadBaseNetworkHeader, 7, []byte{255})...)
	}
	expressions = append(expressions, payloadEquals(expr.PayloadBaseTransportHeader, 0, []byte{byte(kind)})...)
	return accept(expressions...)
}
