package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const noCapabilities = `Name: qwen-check
CapInh: 0000000000000000
CapPrm: 0000000000000000
CapEff: 0000000000000000
CapBnd: 0000000000000000
CapAmb: 0000000000000000
`

func TestVerifyCapabilities(t *testing.T) {
	if err := verifyCapabilities(strings.NewReader(noCapabilities)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range checkedCapabilities {
		for _, mask := range []string{"0000000000000001", "invalid"} {
			status := strings.ReplaceAll(noCapabilities, string(kind)+": 0000000000000000", string(kind)+": "+mask)
			if err := verifyCapabilities(strings.NewReader(status)); err == nil {
				t.Errorf("accepted %s mask %q", kind, mask)
			}
		}
		missing := strings.ReplaceAll(noCapabilities, string(kind)+": 0000000000000000\n", "")
		if err := verifyCapabilities(strings.NewReader(missing)); err == nil {
			t.Errorf("accepted missing %s", kind)
		}
	}
}

func TestVerifyWritableDirectoryCleansUp(t *testing.T) {
	dir := t.TempDir()
	if err := verifyWritableDirectory(dir); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary file left behind: %v, %v", files, err)
	}
}

func TestVerifyEgress(t *testing.T) {
	for _, test := range []struct {
		name                         string
		deniedError, publicError     error
		wantSuccess, wantPublicProbe bool
	}{
		{name: "denied and public", deniedError: &net.OpError{Op: "dial", Net: "tcp", Err: unix.EHOSTUNREACH}, wantSuccess: true, wantPublicProbe: true},
		{name: "permission denied", deniedError: unix.EACCES, wantSuccess: true, wantPublicProbe: true},
		{name: "blocked destination reachable"},
		{name: "timeout is not proof of filtering", deniedError: context.DeadlineExceeded},
		{name: "public internet unavailable", deniedError: unix.EPERM, publicError: fmt.Errorf("dial: %w", unix.ENETUNREACH), wantPublicProbe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var addresses []netip.AddrPort
			err := verifyEgress(t.Context(), func(_ context.Context, address netip.AddrPort) error {
				addresses = append(addresses, address)
				if address == deniedProbeAddress {
					return test.deniedError
				}
				return test.publicError
			})
			if (err == nil) != test.wantSuccess {
				t.Fatalf("egress check: %v", err)
			}
			want := []netip.AddrPort{deniedProbeAddress}
			if test.wantPublicProbe {
				want = append(want, publicProbeAddress)
			}
			if !slices.Equal(addresses, want) {
				t.Fatalf("probed %v, want %v", addresses, want)
			}
		})
	}
}
