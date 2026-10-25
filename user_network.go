package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	networkSubnet                          = "10.0.2.0/24"
	networkGateway                         = "10.0.2.2"
	networkGuest                           = "10.0.2.100"
	networkMTU                             = 1500
	setupNetworkDescriptor setupDescriptor = 5
)

// TAP is created only in the setup process's new user/network namespaces.
// The host-side supervisor receives its descriptor without entering them.
func createNamespaceTAP() (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open TAP device: %w", err)
	}
	file := os.NewFile(uintptr(fd), "tap0")
	request, err := unix.NewIfreq("tap0")
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("NewIfreq(): %w", err)
	}
	request.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, request); err != nil {
		file.Close()
		return nil, fmt.Errorf("create tap0: %w", err)
	}
	if err := configureNamespaceTAP(); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func configureNamespaceTAP() error {
	link, err := netlink.LinkByName("tap0")
	if err != nil {
		return fmt.Errorf("LinkByName(tap0): %w", err)
	}
	if err := netlink.LinkSetMTU(link, networkMTU); err != nil {
		return fmt.Errorf("LinkSetMTU(): %w", err)
	}
	address, err := netlink.ParseAddr(networkGuest + "/24")
	if err != nil {
		return fmt.Errorf("ParseAddr(): %w", err)
	}
	if err := netlink.AddrAdd(link, address); err != nil {
		return fmt.Errorf("AddrAdd(): %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("LinkSetUp(tap0): %w", err)
	}
	loopback, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("LinkByName(lo): %w", err)
	}
	if err := netlink.LinkSetUp(loopback); err != nil {
		return fmt.Errorf("LinkSetUp(lo): %w", err)
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(networkGateway), Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}); err != nil {
		return fmt.Errorf("RouteAdd(): %w", err)
	}
	return nil
}

// The socket carries one descriptor, never untrusted project configuration.
func sendTAP(socket, tap *os.File) error {
	if err := unix.Sendmsg(int(socket.Fd()), []byte{1}, unix.UnixRights(int(tap.Fd())), nil, 0); err != nil {
		return fmt.Errorf("send TAP descriptor: %w", err)
	}
	return nil
}

func receiveTAP(socket *os.File) (*os.File, error) {
	var data [1]byte
	rights := make([]byte, unix.CmsgSpace(4))
	_, count, flags, _, err := unix.Recvmsg(int(socket.Fd()), data[:], rights, unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("receive TAP descriptor: %w", err)
	}
	messages, err := unix.ParseSocketControlMessage(rights[:count])
	if err != nil {
		return nil, fmt.Errorf("parse TAP message: %w", err)
	}
	var descriptors []int
	for _, message := range messages {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			for _, fd := range descriptors {
				unix.Close(fd)
			}
			return nil, fmt.Errorf("parse TAP rights: %w", err)
		}
		descriptors = append(descriptors, fds...)
	}
	if flags&unix.MSG_CTRUNC != 0 || len(descriptors) != 1 || data[0] != 1 {
		for _, fd := range descriptors {
			unix.Close(fd)
		}
		return nil, errors.New("invalid TAP descriptor message")
	}
	if err := unix.SetNonblock(descriptors[0], true); err != nil {
		unix.Close(descriptors[0])
		return nil, fmt.Errorf("SetNonblock(TAP): %w", err)
	}
	return os.NewFile(uintptr(descriptors[0]), "tap0"), nil
}

// A TAP descriptor transfers complete Ethernet frames, like BESS's packet socket.
type tapConnection struct{ *os.File }
type tapAddress string

func (a tapAddress) Network() string       { return "tap" }
func (a tapAddress) String() string        { return string(a) }
func (tapConnection) LocalAddr() net.Addr  { return tapAddress("gateway") }
func (tapConnection) RemoteAddr() net.Addr { return tapAddress("namespace") }

func runUserNetwork(ctx context.Context, network *userNetwork, tap *os.File) error {
	return scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		s.SpawnBg(func() error {
			<-ctx.Done()
			_ = tap.Close()
			return nil
		})
		err := network.AcceptBess(ctx, tapConnection{tap})
		if ctx.Err() != nil {
			return utils.IgnoreCancel(ctx.Err())
		}
		if err != nil {
			return fmt.Errorf("embedded network AcceptBess(): %w", err)
		}
		return errors.New("embedded network packet stream stopped")
	})
}
