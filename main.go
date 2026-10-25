//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/pompon0/pompon0-qwen/utils/scope"
)

func main() {
	// Strip group/other permissions from newly created files and directories
	// (typically 0600 and 0700), keeping state and credentials private. Child
	// processes, including Qwen, inherit this mask; existing files are unchanged.
	syscall.Umask(0o077)
	os.Exit(run())
}

func run() int {
	if code, handled := runInternalHelper(os.Args[1:]); handled {
		return code
	}
	if len(os.Args) == 2 && os.Args[1] == "--isolation-help" {
		fmt.Print(helpText)
		return 0
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	var signalCode int
	code, err := scope.Run1(context.Background(), func(ctx context.Context, tasks scope.Scope) (int, error) {
		tasks.SpawnBg(func() error {
			select {
			case sig := <-signals:
				signalCode = 128 + int(sig.(syscall.Signal))
				tasks.Cancel(context.Canceled)
			case <-ctx.Done():
			}
			return nil
		})
		return launch(ctx, os.Args[1:])
	})
	// The scope joins the signal watcher even when launch returns without a signal.
	if signalCode != 0 {
		return signalCode
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "qwen-isolated:", err)
		}
		return 1
	}
	return code
}

func launch(ctx context.Context, arguments []string) (int, error) {
	invocation, err := parseInvocation(arguments)
	if err != nil {
		return 0, err
	}
	config, err := discoverConfig()
	if err != nil {
		return 0, err
	}
	// A shell must remain available to repair missing or invalid Qwen settings.
	if invocation.Mode != ModeShell {
		config.ModelServer, err = configuredModelServer(config.Home, config.Environment, invocation)
		if err != nil {
			return 0, err
		}
	}
	network, err := discoverNetwork(ctx, config.Tools.IP)
	if err != nil {
		return 0, err
	}
	policy := newNetworkPolicy(network, config.ExtraDenied, config.ModelServer)
	runDir, err := os.MkdirTemp(config.State, "run-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(runDir)
	if err := writeRunFiles(runDir, policy); err != nil {
		return 0, err
	}
	plan, err := buildSandbox(config, invocation, runDir)
	if err != nil {
		return 0, err
	}
	return supervise(ctx, config.Tools, plan, runDir)
}
