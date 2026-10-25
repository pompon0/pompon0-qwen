//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
)

const helpText = `Run Qwen Code in a rootless Linux sandbox from a Git repository.

Usage:
  qwen-isolated [Qwen arguments...]  Start Qwen
  qwen-isolated --shell [args...]    Run Bash inside the sandbox
  qwen-isolated --check              Check filesystem, privileges, and networking
  qwen-isolated --isolation-help     Show this help

Requires bubblewrap, slirp4netns, nftables, iproute2, and util-linux.
Qwen must be installed in the Node prefix selected by your active node.

Configuration, network policy, and packaging:
  https://github.com/pompon0/pompon0-qwen#readme
`

type signalError struct {
	signal syscall.Signal
}

func (e signalError) Error() string { return "received signal: " + e.signal.String() }

func (e signalError) exitCode() int { return 128 + int(e.signal) }

func main() {
	// Strip group/other permissions from newly created files and directories
	// (typically 0600 and 0700), keeping state and credentials private. Child
	// processes, including Qwen, inherit this mask; existing files are unchanged.
	syscall.Umask(0o077)

	err := scope.Run(context.Background(), func(ctx context.Context, s scope.Scope) error {
		if len(os.Args) == 2 && os.Args[1] == "--isolation-help" {
			fmt.Print(helpText)
			return nil
		}
		if handled, err := runInternalHelper(os.Args[1:]); handled {
			return err
		}
		signals := make(chan os.Signal, 1)
		// Handle SIGTERM so cancellation can stop and reap children, join scoped
		// tasks, and remove temporary files before exiting. Linux does not normally
		// kill children when a parent exits: Bubblewrap's --die-with-parent only
		// applies after setup execs it, leaving the unshare/setup phase unprotected.
		// Closing slirp's exit pipe stops networking but does not cover that gap.
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		s.SpawnBg(func() error {
			sig, err := utils.Recv(ctx, signals)
			if err != nil {
				return utils.IgnoreCancel(err)
			}
			return signalError{signal: sig.(syscall.Signal)}
		})
		return launch(ctx, os.Args[1:])
	})
	// The scope joins the signal watcher even when launch returns without a signal.
	if err == nil {
		return
	}
	if interrupted, ok := errors.AsType[signalError](err); ok {
		os.Exit(interrupted.exitCode())
	}
	if exited, ok := errors.AsType[*exec.ExitError](err); ok {
		os.Exit(processExitCode(exited))
	}
	slog.Error("sandbox failed", slog.Any("error", err))
	os.Exit(1)
}

func launch(ctx context.Context, arguments []string) error {
	invocation, err := parseInvocation(arguments)
	if err != nil {
		return err
	}
	config, err := discoverConfig()
	if err != nil {
		return err
	}
	// A shell must remain available to repair missing or invalid Qwen settings.
	if invocation.Mode != ModeShell {
		config.ModelServer, err = configuredModelServer(config.Home, config.Environment, invocation)
		if err != nil {
			return err
		}
	}
	network, err := discoverNetwork(ctx, config.Tools.IP)
	if err != nil {
		return err
	}
	policy := newNetworkPolicy(network, config.ExtraDenied, config.ModelServer)
	runDir, err := os.MkdirTemp(config.State, "run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runDir)
	if err := writeRunFiles(runDir, policy); err != nil {
		return err
	}
	plan, err := buildSandbox(config, invocation, runDir)
	if err != nil {
		return err
	}
	return supervise(ctx, config.Tools, plan, runDir)
}
