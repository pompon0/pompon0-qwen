package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
)

const startupTimeout = 15 * time.Second
const shutdownTimeout = 3 * time.Second

var helperEnvironment = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}

// The supervisor's scope owns each process waiter. Closing done publishes
// err to observers; the supervisor stops all children and joins every waiter
// before returning, including on startup failure and cancellation.
type managedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startProcess(s scope.Scope, cmd *exec.Cmd) (*managedProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &managedProcess{cmd: cmd, done: make(chan struct{})}
	s.SpawnBg(func() error {
		p.err = cmd.Wait()
		// Normalize at the process boundary so callers need no knowledge of
		// exec.ExitError or the kernel's separate signal termination status.
		if exited, ok := errors.AsType[*exec.ExitError](p.err); ok {
			if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				p.err = codeError(128 + int(status.Signal()))
			} else {
				p.err = codeError(exited.ExitCode())
			}
		}
		close(p.done)
		// Record completion without canceling the owner prematurely. Supervision
		// decides whether a process exit is expected using p.done and p.err.
		return nil
	})
	return p, nil
}

func (p *managedProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *managedProcess) stop() {
	if p.exited() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	err := utils.WithTimeout(context.Background(), shutdownTimeout, func(ctx context.Context) error {
		_, _, err := utils.RecvOrClosed(ctx, p.done)
		return err
	})
	if err != nil {
		_ = p.cmd.Process.Kill()
		_, _, _ = utils.RecvOrClosed(context.Background(), p.done)
	}
}

func awaitReady(ctx context.Context, ready *os.File, process *managedProcess, description string, timeout time.Duration) error {
	return scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		result := make(chan error, 1)
		s.SpawnBg(func() error {
			var value [1]byte
			_, err := io.ReadFull(ready, value[:])
			if err == nil && value[0] != '1' {
				err = fmt.Errorf("invalid readiness byte %q", value[0])
			}
			result <- err
			return nil
		})
		// Closing first unblocks ReadFull on every early return. The scope
		// joins the reader before its owning function releases resources.
		defer ready.Close()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err := <-result:
			if err != nil {
				return fmt.Errorf("%s exited before becoming ready: %w", description, err)
			}
			return nil
		case <-process.done:
			return fmt.Errorf("%s exited before becoming ready (%v)", description, process.cmd.ProcessState)
		case <-timer.C:
			return fmt.Errorf("%s startup timed out", description)
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// The setup helper waits until nftables and the embedded network stack are
// ready before execing Bubblewrap. Qwen never runs during the privileged setup phase.
func supervise(ctx context.Context, tools Tools, plan SandboxPlan, runDir string) error {
	return scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		return superviseInScope(ctx, s, tools, plan, runDir)
	})
}

func superviseInScope(ctx context.Context, s scope.Scope, tools Tools, plan SandboxPlan, runDir string) error {
	var opened []*os.File
	defer func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			opened = append(opened, r, w)
		}
		return r, w, err
	}
	readyR, readyW, err := pipe()
	if err != nil {
		return err
	}
	startR, startW, err := pipe()
	if err != nil {
		return err
	}
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("TAP socketpair: %w", err)
	}
	tapReceiver := os.NewFile(uintptr(sockets[0]), "TAP receiver")
	tapSender := os.NewFile(uintptr(sockets[1]), "TAP sender")
	opened = append(opened, tapReceiver, tapSender)
	if err := ctx.Err(); err != nil {
		return err
	}
	launcher, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate setup helper: %w", err)
	}
	args := []string{
		"--user", "--map-root-user", "--net", "--",
		launcher, string(ModeSetup), tools.NFT, filepath.Join(runDir, "policy.nft"), tools.Bubblewrap,
	}
	args = append(args, plan.BubblewrapArguments...)
	args = append(args, "--")
	args = append(args, plan.Workload...)
	cmd := exec.Command(tools.Unshare, args...)
	cmd.Env = helperEnvironment
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// ExtraFiles maps readiness, startup permission, and TAP handoff to 3, 4, 5.
	cmd.ExtraFiles = []*os.File{readyW, startR, tapSender}
	child, err := startProcess(s, cmd)
	if err != nil {
		return fmt.Errorf("start namespace setup: %w", err)
	}
	defer child.stop()
	_ = readyW.Close()
	_ = startR.Close()
	_ = tapSender.Close()
	if err := awaitReady(ctx, readyR, child, "namespace/firewall setup", startupTimeout); err != nil {
		return err
	}
	tap, err := receiveTAP(tapReceiver)
	if err != nil {
		return err
	}
	opened = append(opened, tap)
	_ = tapReceiver.Close()
	network, err := newUserNetwork()
	if err != nil {
		return err
	}
	defer network.Close()
	networkCtx, cancelNetwork := context.WithCancel(ctx)
	defer cancelNetwork()
	s.SpawnBg(func() error {
		err := runUserNetwork(networkCtx, network, tap)
		if err != nil && child.exited() {
			return nil
		}
		return err
	})
	if child.exited() {
		return fmt.Errorf("setup helper exited unexpectedly")
	}
	if _, err := io.WriteString(startW, "go\n"); err != nil {
		return fmt.Errorf("release sandbox startup gate: %w", err)
	}
	_ = startW.Close()
	if _, _, err := utils.RecvOrClosed(ctx, child.done); err != nil {
		return err
	}
	return child.err
}
