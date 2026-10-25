package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

type userNetwork struct {
	stack         *stack.Stack
	networkSwitch *tap.Switch
}

// We own the gVisor stack, so Close stops and joins its protocol workers. Its
// iptables connection-tracking reaper has no exported stop method and remains
// until process exit. The forwarding library also has no join handles for host
// dials or active TCP/UDP/ICMP forwarding tasks and sockets: those may still
// remain until the supervisor exits. We accept process-lifetime cleanup for
// these resources instead of maintaining a fork. No DNS/DHCP services are started.
// Our TAP reader is scoped, canceled, closed, and joined normally.
func newUserNetwork(policy NetworkPolicy) (*userNetwork, error) {
	endpoint, err := tap.NewLinkEndpoint(false, networkMTU, "5a:94:ef:e4:0c:ee", networkGateway, nil)
	if err != nil {
		return nil, fmt.Errorf("NewLinkEndpoint(): %w", err)
	}
	networkSwitch := tap.NewSwitch(false)
	endpoint.Connect(networkSwitch)
	networkSwitch.Connect(endpoint)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	if err := configureUserStack(s, endpoint, policy); err != nil {
		s.Destroy()
		return nil, err
	}
	return &userNetwork{stack: s, networkSwitch: networkSwitch}, nil
}

func configureUserStack(s *stack.Stack, endpoint stack.LinkEndpoint, policy NetworkPolicy) error {
	if err := s.CreateNIC(1, endpoint); err != nil {
		return fmt.Errorf("CreateNIC(): %s", err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(networkGateway).To4()).WithPrefix()}, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("AddProtocolAddress(): %s", err)
	}
	if err := s.SetSpoofing(1, true); err != nil {
		return fmt.Errorf("SetSpoofing(): %s", err)
	}
	if err := s.SetPromiscuousMode(1, true); err != nil {
		return fmt.Errorf("SetPromiscuousMode(): %s", err)
	}
	subnet, err := tcpip.NewSubnet(tcpip.AddrFrom4(netip.MustParsePrefix(networkSubnet).Addr().As4()), tcpip.MaskFromBytes(net.CIDRMask(netip.MustParsePrefix(networkSubnet).Bits(), 32)))
	if err != nil {
		return fmt.Errorf("NewSubnet(): %w", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: 1}})
	rejector, ok := s.NetworkProtocolInstance(ipv4.ProtocolNumber).(stack.RejectIPv4WithHandler)
	if !ok {
		return fmt.Errorf("gVisor IPv4 protocol does not support rejection")
	}
	installEgressFilter(s, policy, rejector)

	// No host-loopback NAT, virtual host aliases, port forwards, HTTP control API,
	// DNS, or DHCP services are exposed. IPv6 has no registered network protocol.
	translation := map[tcpip.Address]tcpip.Address{}
	lock := new(sync.Mutex)
	tcpForwarder := forwarder.TCP(s, translation, lock, false, 128, 30*time.Second)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		// gVisor's Input hook runs before IPv4 reassembly. Admit model-address TCP
		// there, then check the exact port here using gVisor's parsed/reassembled
		// endpoint ID. Noninitial fragments never have to be parsed by our code.
		if !policy.allowsTCP(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort) {
			_ = rejector.SendRejectionError(pkt, stack.RejectIPv4WithICMPAdminProhibited, true)
			return true
		}
		return tcpForwarder.HandlePacket(id, pkt)
	})
	udpForwarder := forwarder.UDP(s, translation, lock, false)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)
	icmpForwarder := forwarder.ICMP(s, translation, lock)
	s.SetTransportProtocolHandler(icmp.ProtocolNumber4, icmpForwarder.HandlePacket)
	return nil
}

func installEgressFilter(s *stack.Stack, policy NetworkPolicy, rejector stack.RejectIPv4WithHandler) {
	table := s.IPTables().GetTable(stack.FilterID, false)
	table.Rules = slices.Clone(table.Rules)
	first := len(table.Rules)
	if model, present := policy.ModelServer.Get(); present {
		filter := stack.EmptyFilter4()
		filter.Dst = tcpip.AddrFrom4(model.Address.As4())
		filter.DstMask = tcpip.AddrFrom4([4]byte{255, 255, 255, 255})
		filter.CheckProtocol, filter.Protocol = true, tcp.ProtocolNumber
		table.Rules = append(table.Rules, stack.Rule{Filter: filter, Target: &stack.AcceptTarget{NetworkProtocol: ipv4.ProtocolNumber}})
	}
	for _, prefix := range policy.Blocked {
		filter := stack.EmptyFilter4()
		filter.Dst = tcpip.AddrFrom4(prefix.Masked().Addr().As4())
		filter.DstMask = tcpip.AddrFrom4Slice(net.CIDRMask(prefix.Bits(), 32))
		table.Rules = append(table.Rules, stack.Rule{Filter: filter, Target: &stack.RejectIPv4Target{Handler: rejector, RejectWith: stack.RejectIPv4WithICMPAdminProhibited}})
	}
	last := len(table.Rules)
	table.Rules = append(table.Rules, stack.Rule{Filter: stack.EmptyFilter4(), Target: &stack.AcceptTarget{NetworkProtocol: ipv4.ProtocolNumber}})
	table.BuiltinChains[stack.Input], table.Underflows[stack.Input] = first, last
	s.IPTables().ReplaceTable(stack.FilterID, table, false)
}

func (n *userNetwork) AcceptBess(ctx context.Context, conn net.Conn) error {
	return n.networkSwitch.Accept(ctx, conn, types.BessProtocol)
}
func (n *userNetwork) Close() { n.stack.Destroy() }
