package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// Helpers run in different privilege contexts and are dispatched before normal
// CLI setup. They must never load project code, Qwen configuration, or hooks.
type helperMode string

const (
	helperSetup        helperMode = "--internal-net-setup"
	helperCheck        helperMode = "--internal-check"
	helperUserNSProbe  helperMode = "--internal-userns-probe"
	sandboxCheckerPath            = "/opt/qwen-isolated-check"
)

// exec.Cmd.ExtraFiles maps the setup pipes to these child descriptors.
type setupDescriptor uintptr

const (
	setupReadyDescriptor setupDescriptor = 3
	setupStartDescriptor setupDescriptor = 4
)

func runInternalHelper(arguments []string) (code int, handled bool) {
	if len(arguments) == 0 {
		return 0, false
	}
	var err error
	switch helperMode(arguments[0]) {
	case helperSetup:
		var setup namespaceSetup
		setup, err = parseNamespaceSetup(arguments[1:])
		if err == nil {
			err = setup.run()
		}
	case helperCheck:
		if len(arguments) != 2 {
			err = errors.New("internal check requires a host canary path")
		} else {
			err = runIsolationChecks(context.Background(), arguments[1], os.Stdout)
		}
	case helperUserNSProbe:
		if len(arguments) != 1 {
			err = errors.New("internal namespace probe takes no arguments")
		}
	default:
		return 0, false
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "qwen-isolated:", err)
		return 1, true
	}
	return 0, true
}

// namespaceSetup holds only trusted supervisor inputs. SandboxArguments starts
// with the Bubblewrap executable and is passed directly to exec, never a shell.
type namespaceSetup struct {
	NFTExecutable    string
	PolicyFile       string
	SandboxArguments []string
}

func parseNamespaceSetup(arguments []string) (namespaceSetup, error) {
	if len(arguments) < 3 {
		return namespaceSetup{}, errors.New("internal setup requires nft, policy, and a sandbox command")
	}
	return namespaceSetup{NFTExecutable: arguments[0], PolicyFile: arguments[1], SandboxArguments: arguments[2:]}, nil
}

func (descriptor setupDescriptor) open(name string) (*os.File, error) {
	file := os.NewFile(uintptr(descriptor), name)
	if file == nil {
		return nil, fmt.Errorf("missing %s pipe", name)
	}
	if _, err := file.Stat(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("open %s pipe: %w", name, err)
	}
	unix.CloseOnExec(int(descriptor))
	return file, nil
}

func (setup namespaceSetup) run() error {
	ready, err := setupReadyDescriptor.open("setup readiness")
	if err != nil {
		return err
	}
	defer ready.Close()
	start, err := setupStartDescriptor.open("setup permission")
	if err != nil {
		return err
	}
	defer start.Close()
	// This stage runs trusted code only, before any Qwen/project code.
	cmd := exec.Command(setup.NFTExecutable, "-f", setup.PolicyFile)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("load namespace firewall: %w", err)
	}
	if err := awaitSandboxPermission(ready, start); err != nil {
		return err
	}
	// Replace this process so slirp remains attached to the same PID and network
	// namespace. The Go runtime never changes namespaces on a running thread.
	return unix.Exec(setup.SandboxArguments[0], setup.SandboxArguments, os.Environ())
}

func awaitSandboxPermission(ready, start *os.File) error {
	if _, err := io.WriteString(ready, "1"); err != nil {
		return fmt.Errorf("signal firewall readiness: %w", err)
	}
	if err := ready.Close(); err != nil {
		return err
	}
	// Read just one line, with a fixed bound; EOF or any other message fails closed.
	message, err := bufio.NewReader(io.LimitReader(start, 3)).ReadString('\n')
	if closeErr := start.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("wait for sandbox permission: %w", err)
	}
	if message != "go\n" {
		return errors.New("invalid sandbox startup permission")
	}
	return nil
}
