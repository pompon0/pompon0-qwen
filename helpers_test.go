package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/pompon0/pompon0-qwen/utils/scope"
)

// Supervisor tests re-exec this test binary through the same private helper
// protocol as the installed launcher. Production setup runs without Bash.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == string(ModeSetup) || os.Args[1] == string(ModeIsolationProbe) || os.Args[1] == string(ModeUserNSProbe)) {
		command := newCommand(os.Args[1:], runSandbox)
		for _, child := range command.Commands() {
			if child.Name() == string(ModeSetup) {
				child.RunE = func(cmd *cobra.Command, args []string) error {
					setup := namespaceSetup{SandboxArguments: args}
					return setup.run(fakeNamespaceTAP)
				}
			}
		}
		err := command.ExecuteContext(context.Background())
		if err != nil {
			slog.Error("internal helper failed", slog.Any("error", err))
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSandboxStartupPermission(t *testing.T) {
	for _, message := range []string{"go\n", "no\n", "go", ""} {
		t.Run(message, func(t *testing.T) {
			readyR, readyW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer readyR.Close()
			defer readyW.Close()
			startR, startW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer startR.Close()
			defer startW.Close()
			var ready [1]byte
			err = scope.Run(t.Context(), func(_ context.Context, s scope.Scope) error {
				s.Spawn(func() error { return awaitSandboxPermission(readyW, startR) })
				_, readErr := io.ReadFull(readyR, ready[:])
				_, writeErr := io.WriteString(startW, message)
				_ = startW.Close()
				return errors.Join(readErr, writeErr)
			})
			if ready[0] != '1' {
				t.Fatalf("readiness protocol: %q", ready)
			}
			if (err == nil) != (message == "go\n") {
				t.Fatalf("permission %q: %v", message, err)
			}
		})
	}
}

// The synthetic TAP preserves packet boundaries without namespace privileges.
// Keep its peer inherited across setup's exec so it lives until the workload
// exits. The supervisor still runs the actual gVisor networking library.
func fakeNamespaceTAP() (*os.File, error) {
	if os.Getenv("POMPON_TEST_FAIL_TAP") == "1" {
		return nil, errors.New("simulated TAP creation failure")
	}
	if os.Getenv("POMPON_TEST_FAIL_NETWORK") == "1" {
		return os.OpenFile(os.DevNull, os.O_RDWR, 0)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(pair[0])
	return os.NewFile(uintptr(pair[0]), "synthetic TAP"), nil
}
