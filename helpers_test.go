package main

import (
	"io"
	"os"
	"testing"

	"golang.org/x/sync/errgroup"
)

// Supervisor tests re-exec this test binary through the same private helper
// protocol as the installed launcher. Production setup runs without Bash.
func TestMain(m *testing.M) {
	if code, handled := runInternalHelper(os.Args[1:]); handled {
		os.Exit(code)
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
			var tasks errgroup.Group
			tasks.Go(func() error { return awaitSandboxPermission(readyW, startR) })
			var ready [1]byte
			_, readErr := io.ReadFull(readyR, ready[:])
			_, writeErr := io.WriteString(startW, message)
			_ = startW.Close()
			err = tasks.Wait()
			if readErr != nil || writeErr != nil || ready[0] != '1' {
				t.Fatalf("readiness protocol: %v, %v, %q", readErr, writeErr, ready)
			}
			if (err == nil) != (message == "go\n") {
				t.Fatalf("permission %q: %v", message, err)
			}
		})
	}
}
