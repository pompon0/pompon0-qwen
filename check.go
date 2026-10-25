package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const sandboxCheckerPath = "/opt/qwen-isolated-check"

type capabilityKind string
type capabilityMask uint64

var checkedCapabilities = [...]capabilityKind{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"}

func runIsolationChecks(ctx context.Context, canary string, output io.Writer) error {
	if _, err := os.Stat(canary); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("FAIL: host canary visible")
		}
		return fmt.Errorf("check host canary: %w", err)
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs("/usr", &filesystem); err != nil {
		return fmt.Errorf("inspect /usr: %w", err)
	}
	if filesystem.Flags&unix.ST_RDONLY == 0 {
		return errors.New("FAIL: /usr is not read-only")
	}
	status, err := os.Open("/proc/self/status")
	if err != nil {
		return err
	}
	err = verifyCapabilities(status)
	_ = status.Close()
	if err != nil {
		return err
	}
	if err := verifyNoUserNamespaces(ctx); err != nil {
		return err
	}
	for _, path := range []string{"/workspace", os.Getenv("HOME")} {
		if err := verifyWritableDirectory(path); err != nil {
			return err
		}
	}
	for _, executable := range []string{"/opt/node/bin/node", "/opt/node/bin/qwen"} {
		cmd := exec.CommandContext(ctx, executable, "--version")
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("check %s version: %w", executable, err)
		}
	}
	if err := verifyEgress(ctx, probeTCP); err != nil {
		return err
	}
	fmt.Fprintln(output, "PASS: denied destination rejected; public IPv4 TCP works.")
	fmt.Fprintln(output, "PASS: host canary hidden; /usr read-only; writable project/home; no capabilities or further user namespaces.")
	return nil
}

func verifyCapabilities(status io.Reader) error {
	seen := make(map[capabilityKind]capabilityMask, len(checkedCapabilities))
	scanner := bufio.NewScanner(status)
	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		kind := capabilityKind(name)
		for _, expected := range checkedCapabilities {
			if kind != expected {
				continue
			}
			bits, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			if err != nil {
				return fmt.Errorf("FAIL: invalid %s capability mask", kind)
			}
			seen[kind] = capabilityMask(bits)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, kind := range checkedCapabilities {
		bits, present := seen[kind]
		if !present {
			return fmt.Errorf("FAIL: %s capability mask missing", kind)
		}
		if bits != 0 {
			return fmt.Errorf("FAIL: %s is nonzero", kind)
		}
	}
	return nil
}

func verifyNoUserNamespaces(ctx context.Context) error {
	// Probe in a child created in a new user namespace, rather than changing
	// namespaces on a thread owned by Go's multithreaded runtime.
	// unshare(CLONE_NEWUSER) requires a single-threaded process, so testing it
	// directly could fail because of Go's runtime instead of sandbox restrictions.
	cmd := exec.CommandContext(ctx, "/usr/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	err := cmd.Run()
	if err == nil {
		return errors.New("FAIL: workload can create further user namespaces")
	}
	if !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.ENOSPC) && !errors.Is(err, unix.EUSERS) {
		return fmt.Errorf("test user namespace restriction: %w", err)
	}
	return nil
}

func verifyWritableDirectory(path string) error {
	file, err := os.CreateTemp(path, ".qwen-isolation-check-*")
	if err != nil {
		return fmt.Errorf("check writable directory %s: %w", path, err)
	}
	defer os.Remove(file.Name())
	return file.Close()
}

type tcpProbe func(context.Context, netip.AddrPort) error

var deniedProbeAddress = netip.MustParseAddrPort("198.18.0.1:9")
var publicProbeAddress = netip.MustParseAddrPort("1.1.1.1:443")

func probeTCP(ctx context.Context, address netip.AddrPort) error {
	dialer := net.Dialer{Timeout: 8 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp4", address.String())
	if err != nil {
		return err
	}
	return connection.Close()
}

func verifyEgress(ctx context.Context, probe tcpProbe) error {
	err := probe(ctx, deniedProbeAddress)
	if err == nil {
		return errors.New("FAIL: blocked destination was reachable")
	}
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EHOSTUNREACH) && !errors.Is(err, unix.ENETUNREACH) {
		return fmt.Errorf("FAIL: blocked destination was not explicitly rejected: %w", err)
	}
	if err := probe(ctx, publicProbeAddress); err != nil {
		return fmt.Errorf("FAIL: public IPv4 TCP: %w", err)
	}
	return nil
}
