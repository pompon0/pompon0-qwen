package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/pompon0/pompon0-qwen/utils/scope"
)

// Supervisor tests re-exec this test binary through the same private helper
// protocol as the installed launcher. Production setup runs without Bash.
func TestMain(m *testing.M) {
	if handled, err := runInternalHelper(os.Args[1:]); handled {
		if err != nil {
			slog.Error("internal helper failed", slog.Any("error", err))
		}
		os.Exit(processExitCode(err))
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
