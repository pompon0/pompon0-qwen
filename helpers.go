package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

const sandboxCheckerPath = "/opt/qwen-isolated-check"

// exec.Cmd.ExtraFiles maps the setup pipes to these child descriptors.
type setupDescriptor uintptr

const (
	setupReadyDescriptor setupDescriptor = 3
	setupStartDescriptor setupDescriptor = 4
)

// namespaceSetup holds only trusted supervisor inputs. SandboxArguments starts
// with the Bubblewrap executable and is passed directly to exec, never a shell.
type namespaceSetup struct {
	NFTExecutable    string
	PolicyFile       string
	SandboxArguments []string
}

func (descriptor setupDescriptor) open(name string) (*os.File, error) {
	// Inherited pipes must be pollable so closing them interrupts scoped reads.
	if err := unix.SetNonblock(int(descriptor), true); err != nil {
		return nil, fmt.Errorf("set %s nonblocking: %w", name, err)
	}
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

func (setup namespaceSetup) run(createTAP func() (*os.File, error)) error {
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
	networkSocket, err := setupNetworkDescriptor.open("TAP handoff")
	if err != nil {
		return err
	}
	defer networkSocket.Close()
	// This stage runs trusted code only, before any Qwen/project code.
	cmd := exec.Command(setup.NFTExecutable, "-f", setup.PolicyFile)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("load namespace firewall: %w", err)
	}
	tap, err := createTAP()
	if err != nil {
		return fmt.Errorf("createNamespaceTAP(): %w", err)
	}
	defer tap.Close()
	if err := sendTAP(networkSocket, tap); err != nil {
		return err
	}
	if err := networkSocket.Close(); err != nil {
		return err
	}
	if err := awaitSandboxPermission(ready, start); err != nil {
		return err
	}
	// Replace this process while preserving the configured network namespace.
	// The Go runtime never changes namespaces on a running thread.
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
