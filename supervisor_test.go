package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
)

// Fake host tools exercise the real pipe/exec handshake without namespaces,
// network privileges, Qwen, or access to any host project.
func fakeTools(t *testing.T, nftFails, slirpFails bool) (Tools, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/bash\nset -eu\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tools := Tools{Bash: "/bin/bash"}
	tools.Unshare = write("unshare", `while [[ $1 != -- ]]; do shift; done; shift; exec "$@"`)
	tools.Bubblewrap = write("bwrap", `while [[ $1 != -- ]]; do shift; done; shift; exec "$@"`)
	nftBody := "printf ready > '" + filepath.Join(dir, "firewall-ready") + "'\n"
	if nftFails {
		nftBody = "exit 7\n"
	}
	tools.NFT = write("nft", nftBody)
	slirpBody := "echo $$ > '" + filepath.Join(dir, "slirp.pid") + "'\n[[ -f '" + filepath.Join(dir, "firewall-ready") + "' ]]\nprintf 1 >&3\nexec 3>&-\n"
	if slirpFails {
		slirpBody += "while [[ ! -f '" + filepath.Join(dir, "workload-started") + "' ]]; do sleep 0.01; done\necho 'simulated network failure' >&2\nexit 8\n"
	} else {
		slirpBody += "read -r value <&4 || true\n"
	}
	tools.Slirp = write("slirp", slirpBody)
	return tools, dir
}

func TestSupervisorSuccessfulHandshakeAndExitStatus(t *testing.T) {
	tools, dir := fakeTools(t, false, false)
	plan := SandboxPlan{Workload: []string{"/bin/bash", "-c", "test -f '" + filepath.Join(dir, "firewall-ready") + "'; exit 23"}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := supervise(ctx, tools, plan, dir)
	if exited, ok := errors.AsType[*exec.ExitError](err); !ok || exited.ExitCode() != 23 {
		t.Fatalf("expected workload exit status 23, got %v", err)
	}
}

func TestSupervisorFirewallFailureDoesNotLaunchWorkload(t *testing.T) {
	tools, dir := fakeTools(t, true, false)
	marker := filepath.Join(dir, "workload-started")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := supervise(ctx, tools, SandboxPlan{Workload: []string{"/usr/bin/touch", marker}}, dir)
	if err == nil {
		t.Fatal("ignored firewall setup failure")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("workload ran despite firewall failure")
	}
}

func TestSupervisorNetworkFailureStopsWorkload(t *testing.T) {
	tools, dir := fakeTools(t, false, true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := supervise(ctx, tools, SandboxPlan{Workload: []string{"/bin/bash", "-c", "touch '" + filepath.Join(dir, "workload-started") + "'; exec sleep 30"}}, dir)
	if err == nil || !strings.Contains(err.Error(), "slirp4netns stopped") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSupervisorCancellationReapsBothChildren(t *testing.T) {
	tools, dir := fakeTools(t, false, false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pidPath := filepath.Join(dir, "workload.pid")
	err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		s.SpawnBg(func() error {
			if err := utils.WaitFor(ctx, 10*time.Millisecond, func() bool {
				data, err := os.ReadFile(pidPath)
				return err == nil && len(data) > 0
			}); err != nil {
				return err
			}
			cancel()
			return nil
		})
		return supervise(ctx, tools, SandboxPlan{Workload: []string{"/bin/bash", "-c", "echo $$ > '" + pidPath + "'; exec sleep 30"}}, dir)
	})
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	for _, path := range []string{pidPath, filepath.Join(dir, "slirp.pid")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("child %d was not reaped before supervisor returned: %v", pid, err)
		}
	}
}

func TestAwaitReadyTimeoutAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}
		err = scope.Run(t.Context(), func(_ context.Context, s scope.Scope) error {
			process, err := startProcess(s, exec.Command("/bin/sleep", "30"))
			if err != nil {
				return err
			}
			defer process.stop()
			return awaitReady(ctx, r, process, "test helper", 20*time.Millisecond)
		})
		cancel()
		_ = w.Close()
		if err == nil {
			t.Fatal("helper became ready without a readiness byte")
		}
	}
}
