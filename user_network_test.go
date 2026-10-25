package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/pompon0/pompon0-qwen/utils/scope"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestTAPDescriptorHandoff(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	sender := os.NewFile(uintptr(pair[0]), "sender")
	receiver := os.NewFile(uintptr(pair[1]), "receiver")
	defer sender.Close()
	defer receiver.Close()
	original, err := os.CreateTemp(t.TempDir(), "descriptor")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if err := sendTAP(sender, original); err != nil {
		t.Fatal(err)
	}
	received, err := receiveTAP(receiver)
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	if _, err := received.WriteString("descriptor survived handoff"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(original.Name())
	if err != nil || string(contents) != "descriptor survived handoff" {
		t.Fatalf("received descriptor: %q, %v", contents, err)
	}
	if _, err := unix.Write(int(sender.Fd()), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if file, err := receiveTAP(receiver); err == nil {
		file.Close()
		t.Fatal("accepted message without a descriptor")
	}
}

// Exercise the in-process stack and its host TCP/UDP sockets using synthetic Ethernet
// frames. Firewall policy is tested separately; no internet access is needed.
func TestEmbeddedNetworkTCPAndUDP(t *testing.T) {
	hostIP := hostTestIPv4(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return err
		}
		tap := os.NewFile(uintptr(pair[0]), "supervisor TAP")
		peer := os.NewFile(uintptr(pair[1]), "guest TAP")
		defer tap.Close()
		defer peer.Close()
		guest, err := net.FileConn(peer)
		if err != nil {
			return err
		}
		defer guest.Close()
		if err := guest.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		network, err := newUserNetwork()
		if err != nil {
			return err
		}
		defer network.Close()
		networkCtx, cancelNetwork := context.WithCancel(ctx)
		defer cancelNetwork()
		s.SpawnBg(func() error { return runUserNetwork(networkCtx, network, tap) })

		guestMAC, err := net.ParseMAC("5a:94:ef:e4:0c:ef")
		if err != nil {
			return err
		}
		gatewayMAC, err := net.ParseMAC("5a:94:ef:e4:0c:ee")
		if err != nil {
			return err
		}
		ethernet := &layers.Ethernet{SrcMAC: guestMAC, DstMAC: gatewayMAC, EthernetType: layers.EthernetTypeIPv4}
		arpEthernet := &layers.Ethernet{SrcMAC: guestMAC, DstMAC: net.HardwareAddr{255, 255, 255, 255, 255, 255}, EthernetType: layers.EthernetTypeARP}
		arp := &layers.ARP{AddrType: layers.LinkTypeEthernet, Protocol: layers.EthernetTypeIPv4, HwAddressSize: 6, ProtAddressSize: 4, Operation: layers.ARPRequest,
			SourceHwAddress: guestMAC, SourceProtAddress: net.ParseIP(networkGuest).To4(), DstHwAddress: make([]byte, 6), DstProtAddress: net.ParseIP(networkGateway).To4()}
		if err := writeTestFrame(guest, arpEthernet, arp); err != nil {
			return err
		}
		response, err := readTestFrame(guest)
		if err != nil {
			return err
		}
		reply, ok := response.Layer(layers.LayerTypeARP).(*layers.ARP)
		if !ok || reply.Operation != layers.ARPReply {
			return fmt.Errorf("missing gateway ARP reply: %v", response)
		}

		udpServer, err := net.ListenPacket("udp4", net.JoinHostPort(hostIP, "0"))
		if err != nil {
			return err
		}
		defer udpServer.Close()
		s.SpawnBg(func() error {
			var data [128]byte
			n, address, err := udpServer.ReadFrom(data[:])
			if err != nil {
				return err
			}
			_, err = udpServer.WriteTo(data[:n], address)
			return err
		})
		ip := &layers.IPv4{Version: 4, TTL: 64, SrcIP: net.ParseIP(networkGuest), DstIP: net.ParseIP(hostIP), Protocol: layers.IPProtocolUDP}
		udp := &layers.UDP{SrcPort: 40000, DstPort: layers.UDPPort(udpServer.LocalAddr().(*net.UDPAddr).Port)}
		if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
			return err
		}
		payload := []byte("udp through embedded stack")
		if err := writeTestFrame(guest, ethernet, ip, udp, gopacket.Payload(payload)); err != nil {
			return err
		}
		response, err = readTestFrame(guest)
		if err != nil {
			return err
		}
		udpReply, ok := response.Layer(layers.LayerTypeUDP).(*layers.UDP)
		if !ok || !slices.Equal(udpReply.Payload, payload) {
			return fmt.Errorf("invalid UDP reply: %v", response)
		}

		tcpServer, err := net.Listen("tcp4", net.JoinHostPort(hostIP, "0"))
		if err != nil {
			return err
		}
		defer tcpServer.Close()
		s.SpawnBg(func() error {
			connection, err := tcpServer.Accept()
			if err != nil {
				return err
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			data := make([]byte, len("tcp request"))
			if _, err := io.ReadFull(connection, data); err != nil {
				return err
			}
			if string(data) != "tcp request" {
				return fmt.Errorf("invalid TCP request %q", data)
			}
			_, err = io.WriteString(connection, "tcp reply")
			return err
		})
		ip.Protocol = layers.IPProtocolTCP
		tcp := &layers.TCP{SrcPort: 40001, DstPort: layers.TCPPort(tcpServer.Addr().(*net.TCPAddr).Port), Seq: 100, SYN: true, Window: 65535}
		if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
			return err
		}
		if err := writeTestFrame(guest, ethernet, ip, tcp); err != nil {
			return err
		}
		response, err = readTestFrame(guest)
		if err != nil {
			return err
		}
		synAck, ok := response.Layer(layers.LayerTypeTCP).(*layers.TCP)
		if !ok || !synAck.SYN || !synAck.ACK {
			return fmt.Errorf("invalid TCP SYN reply: %v", response)
		}
		tcp.Seq, tcp.Ack, tcp.SYN, tcp.ACK = 101, synAck.Seq+1, false, true
		if err := writeTestFrame(guest, ethernet, ip, tcp); err != nil {
			return err
		}
		tcp.PSH = true
		if err := writeTestFrame(guest, ethernet, ip, tcp, gopacket.Payload("tcp request")); err != nil {
			return err
		}
		for {
			response, err = readTestFrame(guest)
			if err != nil {
				return err
			}
			reply, ok := response.Layer(layers.LayerTypeTCP).(*layers.TCP)
			if !ok {
				return fmt.Errorf("invalid TCP response: %v", response)
			}
			if len(reply.Payload) == 0 {
				continue
			}
			if string(reply.Payload) != "tcp reply" {
				return fmt.Errorf("invalid TCP payload %q", reply.Payload)
			}
			break
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeTestFrame(conn net.Conn, contents ...gopacket.SerializableLayer) error {
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, contents...); err != nil {
		return err
	}
	_, err := conn.Write(buffer.Bytes())
	return err
}
func readTestFrame(conn net.Conn) (gopacket.Packet, error) {
	buffer := make([]byte, 65536)
	n, err := conn.Read(buffer)
	if err != nil {
		return nil, err
	}
	return gopacket.NewPacket(buffer[:n], layers.LayerTypeEthernet, gopacket.Default), nil
}
func hostTestIPv4(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.String()
		}
	}
	t.Skip("packet forwarding test needs a non-loopback host IPv4 address")
	return ""
}

// This exercises Linux TAP creation and routes when the host exposes TUN/TAP.
// The packet tests above remain runnable in environments without that device.
func TestRootlessNamespaceTAP(t *testing.T) {
	if os.Getenv("POMPON_TEST_TAP_NAMESPACE") == "1" {
		file, err := createNamespaceTAP()
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		link, err := netlink.LinkByName("tap0")
		if err != nil {
			t.Fatal(err)
		}
		if link.Attrs().MTU != networkMTU {
			t.Fatalf("unexpected MTU: %d", link.Attrs().MTU)
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatal(err)
		}
		if len(addresses) != 1 || addresses[0].IP.String() != networkGuest {
			t.Fatalf("unexpected guest addresses: %v", addresses)
		}
		routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range routes {
			if route.Gw.String() == networkGateway {
				return
			}
		}
		t.Fatalf("default gateway missing: %v", routes)
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skipf("TUN/TAP device unavailable: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/usr/bin/unshare", "--user", "--map-root-user", "--net", "--", executable, "-test.run=^TestRootlessNamespaceTAP$")
	cmd.Env = append(os.Environ(), "POMPON_TEST_TAP_NAMESPACE=1")
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "unshare failed: Operation not permitted") {
		t.Skip("user/network namespaces unavailable")
	}
	if err != nil {
		t.Fatalf("rootless TAP setup: %v\n%s", err, output)
	}
}
