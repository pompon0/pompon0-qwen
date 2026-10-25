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
func fakeTools(t *testing.T) (Tools, string) {
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
	var tools Tools
	tools.Unshare = write("unshare", `while [[ $1 != -- ]]; do shift; done; shift; exec "$@"`)
	tools.Bubblewrap = write("bwrap", `while [[ $1 != -- ]]; do shift; done; shift; exec "$@"`)
	return tools, dir
}

func TestSupervisorSuccessfulHandshakeAndExitStatus(t *testing.T) {
	tools, _ := fakeTools(t)
	plan := SandboxPlan{Workload: []string{"/bin/bash", "-c", "exit 23"}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := supervise(ctx, tools, plan, NetworkPolicy{})
	if exited, ok := errors.AsType[codeError](err); !ok || exited.exitCode() != 23 {
		t.Fatalf("expected workload exit status 23, got %v", err)
	}
}

func TestProcessWaitNormalizesExitStatus(t *testing.T) {
	for _, test := range []struct {
		name, command string
		want          int
	}{
		{name: "success", command: "exit 0", want: 0},
		{name: "failure", command: "exit 23", want: 23},
		{name: "signal", command: "kill -TERM $$", want: 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := scope.Run(t.Context(), func(ctx context.Context, s scope.Scope) error {
				process, err := startProcess(s, exec.Command("/bin/bash", "-c", test.command))
				if err != nil {
					return err
				}
				defer process.stop()
				if _, _, err := utils.RecvOrClosed(ctx, process.done); err != nil {
					return err
				}
				return process.err
			})
			if test.want == 0 {
				if err != nil {
					t.Fatalf("successful process returned %v", err)
				}
				return
			}
			if exited, ok := errors.AsType[codeError](err); !ok || exited.exitCode() != test.want {
				t.Fatalf("expected codeError(%d), got %v", test.want, err)
			}
		})
	}
}

func TestSupervisorTAPFailureDoesNotLaunchWorkload(t *testing.T) {
	tools, dir := fakeTools(t)
	if err := os.WriteFile(tools.Unshare, []byte("#!/bin/bash\nset -eu\nexport POMPON_TEST_FAIL_TAP=1\nwhile [[ $1 != -- ]]; do shift; done; shift; exec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "workload-started")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := supervise(ctx, tools, SandboxPlan{Workload: []string{"/usr/bin/touch", marker}}, NetworkPolicy{})
	if err == nil {
		t.Fatal("ignored TAP setup failure")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("workload ran despite TAP failure")
	}
}

func TestSupervisorNetworkFailureStopsWorkload(t *testing.T) {
	tools, _ := fakeTools(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := os.WriteFile(tools.Unshare, []byte("#!/bin/bash\nset -eu\nexport POMPON_TEST_FAIL_NETWORK=1\nwhile [[ $1 != -- ]]; do shift; done; shift; exec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := supervise(ctx, tools, SandboxPlan{Workload: []string{"/bin/sleep", "30"}}, NetworkPolicy{})
	if err == nil || !strings.Contains(err.Error(), "embedded network") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSupervisorCancellationReapsChild(t *testing.T) {
	tools, dir := fakeTools(t)
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
		return supervise(ctx, tools, SandboxPlan{Workload: []string{"/bin/bash", "-c", "echo $$ > '" + pidPath + "'; exec sleep 30"}}, NetworkPolicy{})
	})
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	for _, path := range []string{pidPath} {
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
