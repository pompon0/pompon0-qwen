package main

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

type containerName string

type SandboxPlan struct {
	Name         containerName
	RunArguments []string
	Workload     []string
}

func runSandbox(ctx context.Context, invocation Args) error {
	config, err := discoverConfig()
	if err != nil {
		return err
	}
	config.ModelServer, err = configuredModelServer(config.Home, config.Environment, invocation)
	if err != nil {
		return err
	}
	network, err := discoverNetwork(ctx, config.Tools.IP)
	if err != nil {
		return err
	}
	policy := newNetworkPolicy(network, config.ExtraDenied, config.ModelServer)
	runDir, err := os.MkdirTemp(config.State, "run-")
	if err != nil {
		return fmt.Errorf("MkdirTemp(): %w", err)
	}
	defer os.RemoveAll(runDir)
	if err := writeRunFiles(runDir); err != nil {
		return err
	}
	plan, err := buildSandbox(config, invocation, runDir)
	if err != nil {
		return err
	}
	return supervise(ctx, config.Tools, plan, policy)
}

func buildSandbox(config Config, invocation Args, runDir string) (SandboxPlan, error) {
	name := containerName("qwen-isolated-" + strings.ToLower(rand.Text()))
	args := []string{
		"run", "--detach", "--rm", "--name", string(name), "--rootfs", "--read-only",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--security-opt=label=disable",
		// Rootless Podman's namespace root maps to the invoking host user. A single
		// UID/GID mapping suffices: the rootfs has no image or other user accounts.
		"--userns=host", "--user=0:0", "--pid=private", "--ipc=private", "--uts=private",
		"--network=pasta:--ipv4-only,-a,10.0.2.100,-n,24,-g,10.0.2.2,--no-map-gw",
		"--dns=none", "--no-hosts", "--http-proxy=false", "--log-driver=none", "--init",
		"--tmpfs=/tmp:rw,nosuid,nodev", "--tmpfs=/run:rw,nosuid,nodev",
		"--workdir", config.WorkingDirectory,
	}
	mounts := []struct {
		source, target string
		writable       bool
	}{
		{"/usr", "/usr", false},
		{config.Project, "/workspace", true}, {config.Home, "/home/qwen", true},
	}
	for _, mount := range mounts {
		args = append(args, "--mount", bindMount(mount.source, mount.target, mount.writable))
	}
	for _, path := range []string{"/etc/nsswitch.conf", "/etc/ssl/certs", "/etc/ca-certificates"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--mount", bindMount(path, path, false))
		} else if !os.IsNotExist(err) {
			return SandboxPlan{}, fmt.Errorf("Stat(%s): %w", path, err)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(config.Environment)) {
		args = append(args, "--env", key+"="+config.Environment[key])
	}
	var workload []string
	switch invocation.Mode {
	case ModeQwen:
		workload = qwenCommand(invocation.Version, invocation.Arguments)
	case ModeIsolationCheck:
		// The diagnostic binary is exposed only for the explicit isolation check.
		if config.Launcher == "" {
			return SandboxPlan{}, fmt.Errorf("isolation-check requires the launcher executable")
		}
		args = append(args, "--mount", bindMount(config.Launcher, sandboxCheckerPath, false))
		workload = []string{sandboxCheckerPath, string(ModeIsolationProbe), "--qwen-version", invocation.Version.String(), filepath.Join(runDir, "host-canary")}
	default:
		return SandboxPlan{}, fmt.Errorf("unknown sandbox mode %q", invocation.Mode)
	}
	// Start only a trusted idle process. Qwen is executed after firewall setup,
	// with the container's empty capability set rather than setup's privileges.
	// sleep infinity is the container's main command (under --init), keeping it
	// alive independently of Qwen. Qwen's lifetime is shorter than the container's:
	// the supervisor installs the firewall before starting Qwen via podman exec,
	// then removes the container after Qwen exits.
	args = append(args, "--", filepath.Join(runDir, "rootfs"), "/usr/bin/sleep", "infinity")
	return SandboxPlan{Name: name, RunArguments: args, Workload: workload}, nil
}

func bindMount(source, target string, writable bool) string {
	var output strings.Builder
	writer := csv.NewWriter(&output)
	fields := []string{"type=bind", "src=" + source, "dst=" + target}
	if !writable {
		fields = append(fields, "ro=true")
	}
	// strings.Builder writes cannot fail. CSV preserves paths with commas/quotes.
	_ = writer.Write(fields)
	writer.Flush()
	return strings.TrimSuffix(output.String(), "\n")
}

func writeRunFiles(runDir string) error {
	root := filepath.Join(runDir, "rootfs")
	// This is a mount skeleton, not an OS image. Executables and libraries come
	// from read-only host mounts; only the project and Qwen home persist writes.
	for _, directory := range []string{"usr", "etc", "dev", "proc", "sys", "tmp", "run", "workspace", "home/qwen"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			return fmt.Errorf("MkdirAll(%s): %w", directory, err)
		}
	}
	for name, target := range map[string]string{"bin": "usr/bin", "sbin": "usr/bin", "lib": "usr/lib", "lib64": "usr/lib"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			return fmt.Errorf("Symlink(%s): %w", name, err)
		}
	}
	files := map[string]string{"etc/resolv.conf": "nameserver 1.1.1.1\n", "etc/hosts": "127.0.0.1 localhost\n",
		"etc/passwd": "qwen:x:0:0:Qwen:/home/qwen:/usr/bin/bash\n", "etc/group": "qwen:x:0:\n", "etc/hostname": "qwen-isolated\n"}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			return fmt.Errorf("WriteFile(%s): %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, "host-canary"), []byte("host-only\n"), 0o600); err != nil {
		return fmt.Errorf("WriteFile(host-canary): %w", err)
	}
	return nil
}

func interactiveArguments() []string {
	args := []string{"--interactive"}
	if _, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS); err == nil {
		if _, err := unix.IoctlGetTermios(int(os.Stdout.Fd()), unix.TCGETS); err == nil {
			args = append(args, "--tty")
		}
	}
	return args
}
