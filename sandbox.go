package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

type SandboxPlan struct {
	BubblewrapArguments []string
	Workload            []string
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

func buildSandbox(config Config, invocation Args, runDir string) (SandboxPlan, error) {
	args := []string{
		"--unshare-all", "--unshare-user", "--disable-userns", "--share-net",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/bin", "/sbin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib", "/lib64",
		"--ro-bind", config.NodePrefix, "/opt/node",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--tmpfs", "/run",
		"--bind", config.Project, "/workspace", "--bind", config.Home, "/home/qwen",
		"--ro-bind", filepath.Join(runDir, "resolv.conf"), "/etc/resolv.conf",
		"--ro-bind", filepath.Join(runDir, "passwd"), "/etc/passwd",
		"--ro-bind", filepath.Join(runDir, "group"), "/etc/group",
		"--chdir", config.WorkingDirectory,
	}
	for _, path := range []string{"/etc/hosts", "/etc/nsswitch.conf", "/etc/ssl/certs", "/etc/ca-certificates"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		} else if !os.IsNotExist(err) {
			return SandboxPlan{}, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(config.Environment)) {
		args = append(args, "--setenv", name, config.Environment[name])
	}
	var workload []string
	switch invocation.Mode {
	case ModeQwen:
		workload = append([]string{"/opt/node/bin/qwen"}, invocation.Arguments...)
	case ModeIsolationCheck:
		// Check mode explicitly mounts the launcher read-only at a stable
		// path so Go diagnostics work regardless of its installation location.
		if config.Launcher == "" {
			return SandboxPlan{}, fmt.Errorf("isolation-check mode requires the launcher executable")
		}
		args = append(args, "--ro-bind", config.Launcher, sandboxCheckerPath)
		workload = []string{sandboxCheckerPath, string(ModeIsolationProbe), filepath.Join(runDir, "host-canary")}
	default:
		return SandboxPlan{}, fmt.Errorf("unknown sandbox mode %q", invocation.Mode)
	}
	return SandboxPlan{BubblewrapArguments: args, Workload: workload}, nil
}

func writeRunFiles(runDir string, policy NetworkPolicy) error {
	files := map[string]string{
		"policy.nft": policy.NFTables(), "resolv.conf": "nameserver 1.1.1.1\n",
		"passwd": "qwen:x:0:0:Qwen:/home/qwen:/usr/bin/bash\n", "group": "qwen:x:0:\n", "host-canary": "host-only\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(contents), 0o600); err != nil {
			return err
		}
	}
	return nil
}
