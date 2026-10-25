package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

const startupTimeout = 15 * time.Second
const shutdownTimeout = 3 * time.Second

var helperEnvironment = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}

// The supervisor's errgroup owns each process waiter. Closing done publishes
// err to observers; the supervisor stops all children and joins every waiter
// before returning, including on startup failure and cancellation.
type managedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startProcess(tasks *errgroup.Group, cmd *exec.Cmd) (*managedProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &managedProcess{cmd: cmd, done: make(chan struct{})}
	tasks.Go(func() error {
		p.err = cmd.Wait()
		close(p.done)
		// Workload exit codes are results, not group errors. Supervision
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
	if p == nil || p.exited() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func awaitReady(ctx context.Context, ready *os.File, process *managedProcess, description string, timeout time.Duration) error {
	var readers errgroup.Group
	finished := make(chan struct{})
	readers.Go(func() error {
		defer close(finished)
		var value [1]byte
		_, err := io.ReadFull(ready, value[:])
		if err == nil && value[0] != '1' {
			err = fmt.Errorf("invalid readiness byte %q", value[0])
		}
		return err
	})
	defer func() {
		// Closing first unblocks ReadFull on every early return. Join the
		// reader before its owning function releases any resources.
		_ = ready.Close()
		_ = readers.Wait()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-finished:
		if err := readers.Wait(); err != nil {
			return fmt.Errorf("%s exited before becoming ready: %w", description, err)
		}
		return nil
	case <-process.done:
		return fmt.Errorf("%s exited before becoming ready (status %d)", description, processExitCode(process.err))
	case <-timer.C:
		return fmt.Errorf("%s startup timed out", description)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The setup shell waits until both nftables and slirp are ready before execing
// Bubblewrap. Qwen never runs during the privileged setup phase.
func supervise(ctx context.Context, tools Tools, plan SandboxPlan, runDir string) (code int, err error) {
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
		return 0, err
	}
	startR, startW, err := pipe()
	if err != nil {
		return 0, err
	}
	slirpR, slirpW, err := pipe()
	if err != nil {
		return 0, err
	}
	exitR, exitW, err := pipe()
	if err != nil {
		return 0, err
	}
	logPath := filepath.Join(runDir, "slirp.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	var tasks errgroup.Group
	var child, slirp *managedProcess
	defer func() {
		// slirp's exit pipe also closes automatically if the supervisor dies.
		_ = exitW.Close()
		child.stop()
		slirp.stop()
		_ = tasks.Wait()
		if err != nil {
			if contents, readErr := os.ReadFile(logPath); readErr == nil && strings.TrimSpace(string(contents)) != "" {
				fmt.Fprintln(os.Stderr, strings.TrimSpace(string(contents)))
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	args := []string{
		"--user", "--map-root-user", "--net", "--",
		tools.Bash, "--noprofile", "--norc", "-c", setupScript, "qwen-net-setup",
		tools.NFT, filepath.Join(runDir, "policy.nft"), "3", "4", tools.Bubblewrap,
	}
	args = append(args, plan.BubblewrapArguments...)
	args = append(args, "--")
	args = append(args, plan.Workload...)
	cmd := exec.Command(tools.Unshare, args...)
	cmd.Env = helperEnvironment
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// ExtraFiles maps consecutively to child descriptors 3 and 4.
	cmd.ExtraFiles = []*os.File{readyW, startR}
	child, err = startProcess(&tasks, cmd)
	if err != nil {
		return 0, fmt.Errorf("start namespace setup: %w", err)
	}
	_ = readyW.Close()
	_ = startR.Close()
	if err := awaitReady(ctx, readyR, child, "namespace/firewall setup", startupTimeout); err != nil {
		return 0, err
	}
	cmd = exec.Command(tools.Slirp,
		"--configure", "--disable-host-loopback", "--disable-dns",
		"--ready-fd=3", "--exit-fd=4", strconv.Itoa(child.cmd.Process.Pid), "tap0")
	cmd.Env = helperEnvironment
	cmd.Stdout, cmd.Stderr = log, log
	cmd.ExtraFiles = []*os.File{slirpW, exitR}
	slirp, err = startProcess(&tasks, cmd)
	if err != nil {
		return 0, fmt.Errorf("start slirp4netns: %w", err)
	}
	_ = slirpW.Close()
	_ = exitR.Close()
	if err := awaitReady(ctx, slirpR, slirp, "slirp4netns", startupTimeout); err != nil {
		return 0, err
	}
	if child.exited() || slirp.exited() {
		return 0, fmt.Errorf("a setup helper exited unexpectedly")
	}
	if _, err := io.WriteString(startW, "go\n"); err != nil {
		return 0, fmt.Errorf("release sandbox startup gate: %w", err)
	}
	_ = startW.Close()
	select {
	case <-child.done:
		return processExitCode(child.err), nil
	case <-slirp.done:
		// A normal workload exit can race slirp's exit notification.
		if child.exited() {
			return processExitCode(child.err), nil
		}
		return 0, fmt.Errorf("slirp4netns stopped; terminating the sandbox")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitError.ExitCode()
	}
	return 1
}
