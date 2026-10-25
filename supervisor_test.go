package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
)

func fakePodman(t *testing.T) (Tools, string) {
	t.Helper()
	directory := t.TempDir()
	executable := filepath.Join(directory, "podman")
	const script = `#!/bin/bash
set -eu
fixture_dir="${0%/*}"
printf '%s\n' "$*" >> "$fixture_dir/calls"
[[ $1 == --remote=false ]]
shift
case "$1" in
 info) if [[ -f "$fixture_dir/rootful" ]]; then echo false; else echo true; fi ;;
 run) echo test-container ;;
 exec)
  shift
  if [[ $1 == --privileged ]]; then
   cat > "$fixture_dir/rules"
   if [[ -f "$fixture_dir/fail-firewall" ]]; then echo setup-failed >&2; exit 7; fi
  else
   while [[ $1 == --* ]]; do shift; done
   shift
   exec "$@"
  fi ;;
 rm) touch "$fixture_dir/removed" ;;
 *) exit 9 ;;
esac
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return Tools{Podman: executable, NFT: "/usr/bin/nft", IP: "/usr/bin/ip"}, directory
}

func testPlan(workload ...string) SandboxPlan {
	return SandboxPlan{Name: "test-sandbox", RunArguments: []string{"run", "--name", "test-sandbox"}, Workload: workload}
}

func TestPodmanSetupBeforeWorkloadAndExitStatus(t *testing.T) {
	tools, directory := fakePodman(t)
	err := supervise(t.Context(), tools, testPlan("/bin/bash", "-c", "test -f '"+filepath.Join(directory, "rules")+"'; exit 23"), NetworkPolicy{})
	if code, ok := errors.AsType[codeError](err); !ok || code.exitCode() != 23 {
		t.Fatalf("expected workload status 23, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "removed")); err != nil {
		t.Fatal("container was not removed", err)
	}
	calls, err := os.ReadFile(filepath.Join(directory, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "exec --privileged --user=0:0 --interactive test-sandbox /usr/bin/nft --file -") {
		t.Fatalf("missing trusted firewall setup: %s", calls)
	}
}

func TestPodmanSetupFailureDoesNotLaunchWorkload(t *testing.T) {
	tools, directory := fakePodman(t)
	if err := os.WriteFile(filepath.Join(directory, "fail-firewall"), []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "workload")
	err := supervise(t.Context(), tools, testPlan("/usr/bin/touch", marker), NetworkPolicy{})
	if err == nil || !strings.Contains(err.Error(), "install sandbox firewall") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("workload ran before firewall setup")
	}
	if _, err := os.Stat(filepath.Join(directory, "removed")); err != nil {
		t.Fatal("failed container was not removed", err)
	}
}

func TestPodmanCancellationRemovesContainer(t *testing.T) {
	for _, test := range []struct {
		name, prefix string
	}{
		{name: "terminates on SIGTERM"},
		{name: "requires SIGKILL", prefix: "trap '' TERM; "},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools, directory := fakePodman(t)
			pidPath := filepath.Join(directory, "workload.pid")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
				s.SpawnBg(func() error {
					if err := utils.WaitFor(ctx, 10*time.Millisecond, func() bool { _, err := os.Stat(pidPath); return err == nil }); err != nil {
						return err
					}
					cancel()
					return nil
				})
				return supervise(ctx, tools, testPlan("/bin/bash", "-c", test.prefix+"echo $$ > '"+pidPath+"'; exec sleep 30"), NetworkPolicy{})
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected cancellation: %v", err)
			}
			if _, err := os.Stat(filepath.Join(directory, "removed")); err != nil {
				t.Fatal("canceled container was not removed", err)
			}
			data, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("attached process was not reaped: %v", err)
			}
		})
	}
}

func TestPodmanWorkloadExitStatus(t *testing.T) {
	for _, test := range []struct {
		name, command string
		want          int
	}{
		{name: "success", command: "exit 0", want: 0},
		{name: "failure", command: "exit 23", want: 23},
		{name: "signal", command: "kill -TERM $$", want: 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools, directory := fakePodman(t)
			err := supervise(t.Context(), tools, testPlan("/bin/bash", "-c", test.command), NetworkPolicy{})
			if _, err := os.Stat(filepath.Join(directory, "removed")); err != nil {
				t.Fatal("container was not removed", err)
			}
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
