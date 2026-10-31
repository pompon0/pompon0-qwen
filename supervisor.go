package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const controlTimeout = 30 * time.Second
const shutdownTimeout = 3 * time.Second

// Podman owns namespaces, networking, mount setup, and container processes. The
// launcher only orders its commands: trusted idle container, firewall, then Qwen.
func supervise(ctx context.Context, tools Tools, plan SandboxPlan, policy NetworkPolicy) (err error) {
	var setup bytes.Buffer
	if err := json.NewEncoder(&setup).Encode(policy); err != nil {
		return fmt.Errorf("encode firewall policy: %w", err)
	}
	rootless, err := tools.RunPodman(ctx, strings.NewReader(""), "info", "--format", "{{.Host.Security.Rootless}}")
	if err != nil {
		return err
	}
	if rootless != "true" {
		return errors.New("Podman must use a local rootless engine")
	}
	// Cleanup also covers partial create/setup failures. Killing an attached
	// Podman CLI alone does not guarantee that its container has stopped.
	defer func() {
		// RunPodman supplies the timeout; cleanup must outlive cancellation.
		if _, cleanupErr := tools.RunPodman(context.WithoutCancel(ctx), strings.NewReader(""), "rm", "--force", "--ignore", "--time", "3", string(plan.Name)); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove sandbox container: %w", cleanupErr))
		}
	}()
	if _, err := tools.RunPodman(ctx, strings.NewReader(""), plan.RunArguments...); err != nil {
		return err
	}
	// Only this trusted host executable receives setup privileges, before any
	// Qwen/project code runs. Normal exec inherits the container's dropped caps.
	if _, err := tools.RunPodman(ctx, &setup, "exec", "--privileged", "--user=0:0", "--interactive", string(plan.Name), sandboxLauncherPath, string(ModeFirewallSetup)); err != nil {
		return fmt.Errorf("install sandbox firewall: %w", err)
	}
	args := append([]string{"--remote=false", "exec"}, interactiveArguments()...)
	args = append(args, string(plan.Name))
	args = append(args, plan.Workload...)
	cmd := exec.CommandContext(ctx, tools.Podman, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Allow graceful shutdown on cancellation, then force termination after the
	// grace period. Run waits for the CLI to be reaped before container cleanup.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = shutdownTimeout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Normalize at the process boundary so callers need no knowledge of
		// exec.ExitError or the kernel's separate signal termination status.
		if exited, ok := errors.AsType[*exec.ExitError](err); ok {
			if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return codeError(128 + int(status.Signal()))
			}
			return codeError(exited.ExitCode())
		}
		return fmt.Errorf("run Podman workload: %w", err)
	}
	return nil
}
