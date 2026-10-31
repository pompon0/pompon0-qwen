package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPodmanPlan(t *testing.T) {
	config := Config{Project: "/host/project, with spaces", Home: t.TempDir(), WorkingDirectory: "/host/project, with spaces/sub", Tools: Tools{NPM: "/tools/npm"}}
	if err := os.Mkdir(filepath.Join(config.Home, ".qwen"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.Home, ".qwen", "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{ModeQwen, ModeIsolationCheck} {
		plan, err := buildSandbox(config, Args{Mode: mode, Version: "0.26.1", Arguments: []string{"--model", "local"}}, "/run-dir")
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"--rootfs", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges", "--userns=host", "--http-proxy=false"} {
			if !slices.Contains(plan.RunArguments, flag) {
				t.Errorf("missing %s", flag)
			}
		}
		for _, argument := range plan.RunArguments {
			if strings.Contains(argument, "--ipv4-only") || strings.Contains(argument, "--ipv6-only") {
				t.Fatal("sandbox must allow both IP families")
			}
		}
		if !containsSequence(plan.RunArguments, []string{"--", "/run-dir/rootfs", "/usr/bin/sleep", "infinity"}) {
			t.Fatal("container must start only trusted sleep")
		}
		if !containsSequence(plan.RunArguments, []string{"--workdir", config.WorkingDirectory}) {
			t.Fatal("sandbox working directory must retain its host path")
		}
		mounts := map[string][]string{}
		for i, arg := range plan.RunArguments {
			if arg != "--mount" {
				continue
			}
			fields, err := csv.NewReader(strings.NewReader(plan.RunArguments[i+1])).Read()
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range fields {
				if strings.HasPrefix(field, "dst=") {
					mounts[strings.TrimPrefix(field, "dst=")] = fields
				}
			}
		}
		if !slices.Contains(mounts[config.Project], "src="+config.Project) || slices.Contains(mounts[config.Project], "ro=true") {
			t.Fatal("project must remain writable with intact path")
		}
		if !slices.Contains(mounts[filepath.Join(config.Project, ".git")], "src="+filepath.Join(config.Project, ".git")) {
			t.Fatal("repository metadata must be mounted from the project")
		}
		for _, target := range []string{"/usr", filepath.Join(config.Project, ".git"), filepath.Join(config.Home, ".qwen/settings.json")} {
			if !slices.Contains(mounts[target], "ro=true") {
				t.Errorf("%s must be read-only", target)
			}
		}
		if _, mounted := mounts[config.Home]; mounted {
			t.Fatal("host home must not be mounted")
		}
		qwenMount := mounts[filepath.Join(config.Home, ".qwen")]
		if !slices.Contains(qwenMount, "src="+filepath.Join(config.Home, ".local/state/qwen-box")) || slices.Contains(qwenMount, "ro=true") {
			t.Fatal("Qwen runtime state must use a private writable directory")
		}
		cacheMount := mounts[filepath.Join(config.Home, ".cache")]
		if !slices.Contains(cacheMount, "src="+filepath.Join(config.Home, ".cache/qwen-box")) || slices.Contains(cacheMount, "ro=true") {
			t.Fatal("the full cache directory must use the writable launcher cache")
		}
		if mode == ModeQwen {
			if plan.Workload[0] != config.Tools.NPM {
				t.Fatal("Qwen must use the configured npm executable")
			}
			if !containsSequence(plan.Workload, []string{"--package=@qwen-code/qwen-code@0.26.1", "--", "qwen", "--model", "local"}) {
				t.Fatal(plan.Workload)
			}
			if _, present := mounts[sandboxCheckerPath]; present {
				t.Fatal("diagnostic executable leaked into normal sandbox")
			}
		} else {
			if !slices.Contains(mounts[sandboxCheckerPath], "ro=true") {
				t.Fatal("diagnostic executable must be mounted read-only")
			}
			if !containsSequence(plan.Workload, []string{"--qwen-version", "0.26.1"}) {
				t.Fatal("diagnostics must check the selected Qwen version")
			}
		}
	}
}

func TestMinimalRootfs(t *testing.T) {
	directory := t.TempDir()
	if err := writeRunFiles(directory, "/host/home", ModeQwen, ResolvedLocalnet{}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "rootfs")
	for _, name := range []string{"bin", "lib", "lib64"} {
		target, err := os.Readlink(filepath.Join(root, name))
		if err != nil || !strings.HasPrefix(target, "usr/") {
			t.Fatalf("%s: %q, %v", name, target, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "usr"))
	if err != nil || len(entries) != 0 {
		t.Fatal("rootfs must not contain an OS installation")
	}
	if _, err := os.Stat(filepath.Join(directory, "host-canary")); !os.IsNotExist(err) {
		t.Fatalf("normal launches must not create diagnostic files: %v", err)
	}
}

func TestIsolationCheckCreatesHostCanary(t *testing.T) {
	directory := t.TempDir()
	if err := writeRunFiles(directory, "/host/home", ModeIsolationCheck, ResolvedLocalnet{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "host-canary")); err != nil {
		t.Fatal("diagnostics require an existing host canary", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "rootfs", "host-canary")); !os.IsNotExist(err) {
		t.Fatalf("host canary must stay outside the rootfs: %v", err)
	}
}

func containsSequence(haystack, needle []string) bool {
	for i := range len(haystack) - len(needle) + 1 {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}
