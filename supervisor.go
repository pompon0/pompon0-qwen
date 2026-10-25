package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	rootless, err := podmanControl(ctx, tools, strings.NewReader(""), "info", "--format", "{{.Host.Security.Rootless}}")
	if err != nil {
		return err
	}
	if rootless != "true" {
		return errors.New("Podman must use a local rootless engine")
	}
	// Cleanup also covers partial create/setup failures. Killing an attached
	// Podman CLI alone does not guarantee that its container has stopped.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), controlTimeout)
		defer cancel()
		if _, cleanupErr := podmanControl(cleanupCtx, tools, strings.NewReader(""), "rm", "--force", "--ignore", "--time", "3", string(plan.Name)); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove sandbox container: %w", cleanupErr))
		}
	}()
	if _, err := podmanControl(ctx, tools, strings.NewReader(""), plan.RunArguments...); err != nil {
		return err
	}
	// Only this trusted host executable receives setup privileges, before any
	// Qwen/project code runs. Normal exec inherits the container's dropped caps.
	if _, err := podmanControl(ctx, tools, strings.NewReader(policy.NFTables()), "exec", "--privileged", "--user=0:0", "--interactive", string(plan.Name), tools.NFT, "--file", "-"); err != nil {
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

func podmanControl(ctx context.Context, tools Tools, input io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tools.Podman, append([]string{"--remote=false"}, args...)...)
	cmd.Stdin = input
	var output, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podman %s: %w (%s)", args[0], err, strings.TrimSpace(diagnostics.String()))
	}
	return strings.TrimSpace(output.String()), nil
}
