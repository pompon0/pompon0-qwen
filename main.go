//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
)

// codeError carries the shell exit status for a signal or a terminated workload.
type codeError int

func (e codeError) Error() string { return fmt.Sprintf("exit status %d", e) }

func (e codeError) exitCode() int { return int(e) }

func main() {
	// Strip group/other permissions from newly created files and directories
	// (typically 0600 and 0700), keeping state and credentials private. Child
	// processes, including Qwen, inherit this mask; existing files are unchanged.
	syscall.Umask(0o077)

	err := scope.Run(context.Background(), func(ctx context.Context, s scope.Scope) error {
		signals := make(chan os.Signal, 1)
		// Handle SIGTERM so cancellation can stop and reap children, join scoped
		// tasks, and remove temporary files before exiting. Linux does not normally
		// kill children when a parent exits. Podman's container also needs explicit
		// removal if its attached CLI exits before the workload does.
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		s.SpawnBg(func() error {
			sig, err := utils.Recv(ctx, signals)
			if err != nil {
				return utils.IgnoreCancel(err)
			}
			return codeError(128 + int(sig.(syscall.Signal)))
		})
		return run(ctx, os.Args[1:])
	})
	if err != nil {
		slog.Error("sandbox failed", slog.Any("error", err))
		if codeErr, ok := errors.AsType[codeError](err); ok {
			os.Exit(codeErr.exitCode())
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	return newCommand(args, runSandbox).ExecuteContext(ctx)
}
