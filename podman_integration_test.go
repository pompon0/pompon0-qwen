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
			config := Config{Project: filepath.Join(directory, "project"), Home: filepath.Join(directory, "home"), WorkingDirectory: filepath.Join(directory, "project", "nested"), Tools: Tools{Podman: "/usr/bin/podman", NFT: "/usr/bin/nft", IP: "/usr/bin/ip", Node: "/usr/bin/node", NPM: "/usr/bin/npm"}, Environment: map[string]string{}}
			for _, path := range []string{filepath.Join(config.Project, ".git"), config.WorkingDirectory, filepath.Join(config.Home, ".qwen"), filepath.Join(config.cacheDir(), "npm"), config.stateDir(), root} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			config.Environment = sandboxEnvironment(config.Home, func(string) (string, bool) { return "", false })
			for _, name := range []string{".qwen/settings.json", ".qwen/.env", ".qwen/oauth_creds.json", ".qwen/manual-session", "host-secret", ".cache/manual-cache"} {
				if err := os.WriteFile(filepath.Join(config.Home, name), []byte("host data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareQwenState(config.Home, config.stateDir()); err != nil {
				t.Fatal(err)
			}
			config.Environment["POMPON_PODMAN_PROBE"] = "1"
			config.Environment["POMPON_TEST_PROJECT"] = config.Project
			config.Environment["POMPON_TEST_CWD"] = config.WorkingDirectory
			config.Environment["POMPON_TEST_MODEL"] = net.JoinHostPort("model.test", fmt.Sprint(modelAddress.Port()))
			config.Environment["POMPON_TEST_DENIED"] = deniedAddress.String()
			config.Environment["POMPON_TEST_CANARY"] = filepath.Join(root, "host-canary")
			endpoint := ModelEndpoint{Hostname: "model.test", Addresses: []netip.AddrPort{modelAddress}}
			if err := writeRunFiles(root, config.Home, ModeIsolationCheck, endpoint); err != nil {
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
			policy := newNetworkPolicy(HostNetwork{Addresses: []netip.Addr{test.target}}, endpoint.Addresses)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if err := supervise(ctx, config.Tools, plan, policy); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(config.stateDir(), "oauth_creds.json")); err != nil || string(data) != "refreshed" {
				t.Fatalf("private credential refresh did not persist: %q, %v", data, err)
			}
			if data, err := os.ReadFile(filepath.Join(config.Home, ".qwen/oauth_creds.json")); err != nil || string(data) != "host data" {
				t.Fatalf("host credentials changed: %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(config.stateDir(), "tmp/session-probe")); err != nil {
				t.Fatalf("private session data did not persist: %v", err)
			}
			for _, name := range []string{"npm/shared-cache-probe", "go-build/shared-cache-probe"} {
				if data, err := os.ReadFile(filepath.Join(config.cacheDir(), name)); err != nil || string(data) != "cached" {
					t.Fatalf("cache writes did not persist on host: %q, %v", data, err)
				}
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
	if err := unix.Statfs(filepath.Join(os.Getenv("POMPON_TEST_PROJECT"), ".git"), &filesystem); err != nil {
		t.Fatal(err)
	}
	if filesystem.Flags&unix.ST_RDONLY == 0 {
		t.Fatal("repository metadata is writable")
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("POMPON_TEST_PROJECT"), ".git", "sandbox-write-probe"), []byte("probe"), 0o600); !errors.Is(err, unix.EROFS) {
		t.Fatalf("writing repository metadata must fail with EROFS: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if cwd != os.Getenv("POMPON_TEST_CWD") {
		t.Fatalf("sandbox working directory %q differs from host %q", cwd, os.Getenv("POMPON_TEST_CWD"))
	}
	for _, directory := range []string{cwd, filepath.Join(os.Getenv("HOME"), ".cache"), filepath.Join(os.Getenv("HOME"), ".qwen")} {
		if err := verifyWritableDirectory(directory); err != nil {
			t.Fatal(err)
		}
	}
	settings := filepath.Join(os.Getenv("HOME"), ".qwen", "settings.json")
	if data, err := os.ReadFile(settings); err != nil || string(data) != "host data" {
		t.Fatalf("host Qwen settings are not shared: %q, %v", data, err)
	}
	if err := os.WriteFile(settings, []byte("changed"), 0o600); !errors.Is(err, unix.EROFS) {
		t.Fatalf("Qwen settings must be read-only: %v", err)
	}
	qwen := filepath.Join(os.Getenv("HOME"), ".qwen")
	if err := os.WriteFile(filepath.Join(qwen, ".env"), []byte("changed"), 0o600); !errors.Is(err, unix.EROFS) {
		t.Fatalf("Qwen environment must be read-only: %v", err)
	}
	if _, err := os.Stat(filepath.Join(qwen, "manual-session")); !os.IsNotExist(err) {
		t.Fatalf("manual Qwen session exposed: %v", err)
	}
	credential := filepath.Join(qwen, "oauth_creds.json")
	if data, err := os.ReadFile(credential); err != nil || string(data) != "host data" {
		t.Fatalf("credentials were not seeded: %q, %v", data, err)
	}
	// Token refreshes commonly replace a file by rename rather than truncating it.
	if err := os.WriteFile(credential+".tmp", []byte("refreshed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(credential+".tmp", credential); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(qwen, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(qwen, "tmp/session-probe"), []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), "host-secret")); !os.IsNotExist(err) {
		t.Fatalf("unrelated host home file exposed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".cache", "manual-cache")); !os.IsNotExist(err) {
		t.Fatalf("manual host cache exposed: %v", err)
	}
	for _, cache := range []string{os.Getenv("NPM_CONFIG_CACHE"), filepath.Join(os.Getenv("HOME"), ".cache/go-build")} {
		if err := os.MkdirAll(cache, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "shared-cache-probe"), []byte("cached"), 0o600); err != nil {
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
