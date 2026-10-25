package main

import (
	_ "embed"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

//go:embed help.txt
var helpText string

type Mode uint8

const (
	ModeQwen Mode = iota
	ModeShell
	ModeCheck
)

type Invocation struct {
	Mode      Mode
	Arguments []string
}

func parseInvocation(arguments []string) (Invocation, error) {
	invocation := Invocation{Mode: ModeQwen, Arguments: slices.Clone(arguments)}
	if len(arguments) == 0 {
		return invocation, nil
	}
	switch arguments[0] {
	case "--shell":
		invocation.Mode, invocation.Arguments = ModeShell, arguments[1:]
	case "--check":
		if len(arguments) != 1 {
			return Invocation{}, fmt.Errorf("--check does not accept arguments")
		}
		invocation.Mode, invocation.Arguments = ModeCheck, nil
	}
	return invocation, nil
}

type SandboxPlan struct {
	BubblewrapArguments []string
	Workload            []string
}

func buildSandbox(config Config, invocation Invocation, runDir string) (SandboxPlan, error) {
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
	case ModeShell:
		workload = append([]string{"/usr/bin/bash", "--noprofile", "--norc"}, invocation.Arguments...)
	case ModeCheck:
		// The launcher is otherwise unavailable inside the sandbox. Check
		// mode alone mounts it read-only so its Go diagnostics can run there.
		if config.Launcher == "" {
			return SandboxPlan{}, fmt.Errorf("check mode requires the launcher executable")
		}
		args = append(args, "--ro-bind", config.Launcher, sandboxCheckerPath)
		workload = []string{sandboxCheckerPath, string(helperCheck), filepath.Join(runDir, "host-canary")}
	default:
		return SandboxPlan{}, fmt.Errorf("unknown sandbox mode %d", invocation.Mode)
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
