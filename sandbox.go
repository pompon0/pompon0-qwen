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

// runSandbox resolves the allowed endpoint and host network before starting any
// workload. The launcher owns its /tmp run directory and removes it after
// supervision ends; shared Qwen state and caches survive the run.
func runSandbox(ctx context.Context, invocation Args) error {
	config, err := discoverConfig(invocation.Config)
	if err != nil {
		return err
	}
	modelEndpoint, err := config.modelEndpoint(ctx)
	if err != nil {
		return err
	}
	network, err := discoverNetwork(ctx, config.Tools.IP)
	if err != nil {
		return err
	}
	policy := newNetworkPolicy(network, modelEndpoint.Addresses)
	runDir, err := os.MkdirTemp("/tmp", "qwen-box-run-")
	if err != nil {
		return fmt.Errorf("MkdirTemp(): %w", err)
	}
	defer os.RemoveAll(runDir)
	if err := prepareQwenState(config.Home, config.stateDir()); err != nil {
		return err
	}
	if err := writeRunFiles(runDir, config.Home, invocation.Mode, modelEndpoint); err != nil {
		return err
	}
	plan, err := buildSandbox(config, invocation, runDir)
	if err != nil {
		return err
	}
	return supervise(ctx, config.Tools, plan, policy)
}

func buildSandbox(config Config, invocation Args, runDir string) (SandboxPlan, error) {
	name := containerName("qwen-box-" + strings.ToLower(rand.Text()))
	args := []string{
		"run", "--detach", "--rm", "--name", string(name), "--rootfs", "--read-only",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--security-opt=label=disable",
		// Rootless Podman's namespace root maps to the invoking host user. A single
		// UID/GID mapping suffices: the rootfs has no image or other user accounts.
		"--userns=host", "--user=0:0", "--pid=private", "--ipc=private", "--uts=private",
		// Separate guest addresses prevent host model servers being shadowed by
		// pasta's default of copying the host's own addresses into the container.
		"--network=pasta:-a,10.0.2.100,-n,24,-g,10.0.2.2,-a,fd00:2::100,-g,fd00:2::2,--no-map-gw",
		"--dns=none", "--no-hosts", "--http-proxy=false", "--log-driver=none", "--init",
		"--tmpfs=/tmp:rw,nosuid,nodev", "--tmpfs=/run:rw,nosuid,nodev",
		"--workdir", config.WorkingDirectory,
	}
	type mountSpec struct {
		source, target string
		writable       bool
	}
	mounts := []mountSpec{
		{"/usr", "/usr", false},
		{config.Project, config.Project, true},
		// Give every tool a shared launcher-only cache at the conventional path.
		{config.cacheDir(), filepath.Join(config.Home, ".cache"), true},
		{config.stateDir(), filepath.Join(config.Home, ".qwen"), true},
		// Protect repository metadata within the otherwise writable project.
		// A bind mount also supports the .git file used by worktrees/submodules.
		{filepath.Join(config.Project, ".git"), filepath.Join(config.Project, ".git"), false},
	}
	// Protect host configuration while leaving Qwen's sessions, logs, and
	// refreshed credential copies writable in shared launcher-owned state.
	for _, name := range qwenSharedConfiguration {
		path := filepath.Join(config.Home, ".qwen", name)
		if info, err := os.Lstat(path); err == nil {
			if !info.Mode().IsRegular() && !info.IsDir() {
				return SandboxPlan{}, fmt.Errorf("Qwen configuration %s must be a regular file or directory", path)
			}
			mounts = append(mounts, mountSpec{path, path, false})
		} else if !os.IsNotExist(err) {
			return SandboxPlan{}, fmt.Errorf("Lstat(Qwen configuration): %w", err)
		}
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
	environment := config.Environment()
	for _, key := range slices.Sorted(maps.Keys(environment)) {
		args = append(args, "--env", key+"="+environment[key])
	}
	var workload []string
	switch invocation.Mode {
	case ModeQwen:
		workload = config.Tools.qwenCommand(invocation.Version, invocation.Arguments)
	case ModeIsolationCheck:
		// The diagnostic binary is exposed only for the explicit isolation check.
		launcher, err := os.Executable()
		if err != nil {
			return SandboxPlan{}, fmt.Errorf("Executable(): %w", err)
		}
		args = append(args, "--mount", bindMount(launcher, sandboxCheckerPath, false))
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

// writeRunFiles assembles the externally managed rootfs passed to Podman; it
// does not build or extract a container image. It creates mount-point directories,
// /usr symlinks, and generated hosts/DNS/hostname/passwd/group files. Podman
// overlays separate writable tmpfs mounts on /tmp and /run, so their contents do
// not live in this skeleton. The diagnostic host-canary stays outside rootfs.
func writeRunFiles(runDir, home string, mode Mode, endpoint ModelEndpoint) error {
	root := filepath.Join(runDir, "rootfs")
	// This is a mount skeleton, not an OS image. Executables and libraries come
	// from read-only host mounts; project, cache, and shared Qwen state persist writes.
	for _, directory := range []string{"usr", "etc", "dev", "proc", "sys", "tmp", "run", "opt/qwen-box-npm", filepath.Join(home, ".qwen")} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			return fmt.Errorf("MkdirAll(%s): %w", directory, err)
		}
	}
	for name, target := range map[string]string{"bin": "usr/bin", "sbin": "usr/bin", "lib": "usr/lib", "lib64": "usr/lib"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			return fmt.Errorf("Symlink(%s): %w", name, err)
		}
	}
	hosts := "127.0.0.1 localhost\n::1 localhost\n"
	for _, address := range endpoint.Addresses {
		hosts += fmt.Sprintf("%s %s\n", address.Addr(), endpoint.Hostname)
	}
	files := map[string]string{
		"etc/resolv.conf": "nameserver 1.1.1.1\nnameserver 2606:4700:4700::1111\n",
		"etc/hosts":       hosts,
		"etc/passwd":      fmt.Sprintf("qwen:x:0:0:Qwen:%s:/usr/bin/bash\n", home),
		"etc/group":       "qwen:x:0:\n",
		"etc/hostname":    "qwen-box\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			return fmt.Errorf("WriteFile(%s): %w", name, err)
		}
	}
	if mode == ModeIsolationCheck {
		if err := os.WriteFile(filepath.Join(runDir, "host-canary"), []byte("host-only\n"), 0o600); err != nil {
			return fmt.Errorf("WriteFile(host-canary): %w", err)
		}
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
