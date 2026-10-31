package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/pompon0/pompon0-qwen/utils/scope"
)

func TestPolicyUsesFixedNonPublicRanges(t *testing.T) {
	policy := newNetworkPolicy([]netip.AddrPort{netip.MustParseAddrPort("192.168.1.50:11434")})
	for _, value := range []string{"10.0.0.1", "127.0.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "198.18.0.1", "224.0.0.1", "::", "::1", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1", "64:ff9b::a00:1"} {
		if !policyBlocks(policy, netip.MustParseAddr(value)) {
			t.Errorf("policy does not block %s", value)
		}
	}
	for _, address := range []string{"1.1.1.1", "2606:4700:4700::1111", "64:ff9b::101:101", "2001:4860::8889", "12.3.4.5", "2606:4700:1234::1", "11.2.3.4", "9.8.7.6", "2001:4860:abcd::1", "2001:4860::8888", "64:ff9b::908:706"} {
		if policyBlocks(policy, netip.MustParseAddr(address)) {
			t.Fatalf("public internet blocked: %s", address)
		}
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

func TestLocalnetDNSResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		connection, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer connection.Close()
		s.SpawnBg(func() error {
			buffer := make([]byte, 4096)
			for {
				n, peer, err := connection.ReadFrom(buffer)
				if errors.Is(err, net.ErrClosed) {
					return nil
				}
				if err != nil {
					return err
				}
				query := buffer[:n]
				name, offset := "", 12
				for query[offset] != 0 {
					length := int(query[offset])
					name += string(query[offset+1:offset+1+length]) + "."
					offset += 1 + length
				}
				offset++
				kind := binary.BigEndian.Uint16(query[offset:])
				response := append([]byte{}, query[:offset+4]...)
				binary.BigEndian.PutUint16(response[2:4], 0x8180)
				clear(response[6:12])
				answers := uint16(0)
				if name == "local.test." {
					for _, value := range []string{"192.168.1.50", "192.168.1.50", "192.168.1.60", "fd12::50"} {
						address := netip.MustParseAddr(value)
						if (kind == 1 && address.Is4()) || (kind == 28 && address.Is6()) {
							response = append(response, 0xc0, 0x0c)
							response = binary.BigEndian.AppendUint16(response, kind)
							response = binary.BigEndian.AppendUint16(response, 1)
							response = binary.BigEndian.AppendUint32(response, 60)
							response = binary.BigEndian.AppendUint16(response, uint16(len(address.AsSlice())))
							response = append(response, address.AsSlice()...)
							answers++
						}
					}
				} else {
					binary.BigEndian.PutUint16(response[2:4], 0x8183)
				}
				binary.BigEndian.PutUint16(response[6:8], answers)
				if _, err := connection.WriteTo(response, peer); err != nil {
					return err
				}
			}
		})
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, "udp4", connection.LocalAddr().String())
		}}
		var localnet Localnet
		if err := localnet.Set("local.test:8000,local.test:9000,192.168.1.50:8000"); err != nil {
			return err
		}
		resolved, err := localnet.resolve(ctx, resolver)
		if err != nil {
			return err
		}
		wantAddresses := []netip.Addr{netip.MustParseAddr("192.168.1.50"), netip.MustParseAddr("192.168.1.60"), netip.MustParseAddr("fd12::50")}
		if len(resolved) != 6 {
			return fmt.Errorf("unexpected resolved endpoints: %+v", resolved)
		}
		for _, address := range wantAddresses {
			for _, port := range []uint16{8000, 9000} {
				if !slices.Contains(resolved, netip.AddrPortFrom(address, port)) {
					return fmt.Errorf("missing resolved destination %s:%d", address, port)
				}
			}
		}
		if err := localnet.Set("local.test:8000,missing.test:9000"); err != nil {
			return err
		}
		failed, err := localnet.resolve(ctx, resolver)
		if err == nil || len(failed) != 0 {
			return fmt.Errorf("failed DNS lookup must discard the entire whitelist: %+v, %v", failed, err)
		}
		if err := localnet.Set("local.test:8000,z\ninvalid.test:9000"); err != nil {
			return fmt.Errorf("hostname validation must be left to the resolver: %w", err)
		}
		failed, err = localnet.resolve(ctx, resolver)
		if err == nil || len(failed) != 0 {
			return fmt.Errorf("invalid hostname must fail resolution without a partial whitelist: %+v, %v", failed, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalnetResolutionCancellation(t *testing.T) {
	var localnet Localnet
	if err := localnet.Set("local.test:8000"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := localnet.Resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
