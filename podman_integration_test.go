package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPodmanIntegration(t *testing.T) {
	if os.Getenv("POMPON_PODMAN_INTEGRATION") != "1" {
		t.Skip("set POMPON_PODMAN_INTEGRATION=1 to test a live rootless Podman sandbox")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var hostIP netip.Addr
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() {
			hostIP = netip.MustParseAddr(ip.String())
			break
		}
	}
	if !hostIP.IsValid() {
		t.Fatal("no host IPv4 address available")
	}
	for _, test := range []struct {
		name, network string
		bind, target  netip.Addr
		pastaOptions  string
	}{
		{name: "IPv4", network: "tcp4", bind: hostIP, target: hostIP},
		{name: "IPv6", network: "tcp6", bind: netip.IPv6Loopback(), target: netip.MustParseAddr("fd00:57::1"),
			pastaOptions: ",--map-host-loopback,fd00:57::1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := func() *httptest.Server {
				t.Helper()
				listener, err := net.Listen(test.network, net.JoinHostPort(test.bind.String(), "0"))
				if err != nil {
					t.Fatal(err)
				}
				result := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "model endpoint") }))
				result.Listener.Close()
				result.Listener = listener
				result.Start()
				t.Cleanup(result.Close)
				return result
			}
			model, other := server(), server()
			modelAddress := netip.AddrPortFrom(test.target, netip.MustParseAddrPort(model.Listener.Addr().String()).Port())
			deniedAddress := netip.AddrPortFrom(test.target, netip.MustParseAddrPort(other.Listener.Addr().String()).Port())
			directory := t.TempDir()
			root := filepath.Join(directory, "run")
			config := Config{Project: filepath.Join(directory, "project"), Home: filepath.Join(directory, "home"), WorkingDirectory: "/workspace", Tools: Tools{Podman: "/usr/bin/podman", NFT: "/usr/bin/nft", IP: "/usr/bin/ip"}, Environment: sandboxEnvironment(func(string) (string, bool) { return "", false })}
			for _, path := range []string{config.Project, config.Home, root} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			config.Environment["POMPON_PODMAN_PROBE"] = "1"
			config.Environment["POMPON_TEST_MODEL"] = net.JoinHostPort("model.test", fmt.Sprint(modelAddress.Port()))
			config.Environment["POMPON_TEST_DENIED"] = deniedAddress.String()
			config.Environment["POMPON_TEST_CANARY"] = filepath.Join(root, "host-canary")
			endpoint := ModelEndpoint{Hostname: "model.test", Addresses: []netip.AddrPort{modelAddress}}
			if err := writeRunFiles(root, ModeIsolationCheck, endpoint); err != nil {
				t.Fatal(err)
			}
			plan, err := buildSandbox(config, Args{Mode: ModeIsolationCheck}, root)
			if err != nil {
				t.Fatal(err)
			}
			plan.Workload = []string{sandboxCheckerPath, "-test.run=^TestPodmanProbe$", "-test.v"}
			// The test-only IPv6 mapping reaches host loopback through a ULA. This
			// exercises real IPv6 packets without requiring a global host IPv6 route.
			if test.pastaOptions != "" {
				for i, argument := range plan.RunArguments {
					if strings.HasPrefix(argument, "--network=pasta:") {
						plan.RunArguments[i] += test.pastaOptions
					}
				}
			}
			policy := newNetworkPolicy(HostNetwork{Addresses: []netip.Addr{test.target}}, nil, endpoint.Addresses)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if err := supervise(ctx, config.Tools, plan, policy); err != nil {
				t.Fatal(err)
			}
			if err := exec.CommandContext(ctx, config.Tools.Podman, "--remote=false", "container", "exists", string(plan.Name)).Run(); err == nil {
				t.Fatal("sandbox container was left running")
			}
		})
	}
}

func TestPodmanProbe(t *testing.T) {
	if os.Getenv("POMPON_PODMAN_PROBE") != "1" {
		t.Skip("only executed inside the integration sandbox")
	}
	status, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	err = verifyCapabilities(status)
	status.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyNoUserNamespaces(t.Context()); err != nil {
		t.Fatal(err)
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs("/usr", &filesystem); err != nil {
		t.Fatal(err)
	}
	if filesystem.Flags&unix.ST_RDONLY == 0 {
		t.Fatal("host /usr is writable")
	}
	for _, directory := range []string{"/workspace", os.Getenv("HOME")} {
		if err := verifyWritableDirectory(directory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(os.Getenv("POMPON_TEST_CANARY")); !os.IsNotExist(err) {
		t.Fatal("host-only path was exposed", err)
	}
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + os.Getenv("POMPON_TEST_MODEL"))
	if err != nil {
		t.Fatal("selected model endpoint was blocked", err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || strings.TrimSpace(string(contents)) != "model endpoint" {
		t.Fatalf("invalid model response: %q, %v", contents, err)
	}
	denied := netip.MustParseAddrPort(os.Getenv("POMPON_TEST_DENIED"))
	err = probeTCP(t.Context(), denied)
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EHOSTUNREACH) && !errors.Is(err, unix.ENETUNREACH) {
		t.Fatalf("other host port was not explicitly rejected: %v", err)
	}
}
