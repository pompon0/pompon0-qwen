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
	config := Config{Project: "/host/project, with spaces", Home: "/host/home", NodePrefix: "/host/node", WorkingDirectory: "/workspace/sub", Launcher: "/host/launcher", Environment: map[string]string{"HOME": "/home/qwen", "PATH": "/opt/node/bin:/usr/bin"}}
	for _, mode := range []Mode{ModeQwen, ModeIsolationCheck} {
		plan, err := buildSandbox(config, Args{Mode: mode, Arguments: []string{"--model", "local"}}, "/run-dir")
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"--rootfs", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges", "--userns=host", "--http-proxy=false"} {
			if !slices.Contains(plan.RunArguments, flag) {
				t.Errorf("missing %s", flag)
			}
		}
		if !containsSequence(plan.RunArguments, []string{"--", "/run-dir/rootfs", "/usr/bin/sleep", "infinity"}) {
			t.Fatal("container must start only trusted sleep")
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
		if !slices.Contains(mounts["/workspace"], "src="+config.Project) || slices.Contains(mounts["/workspace"], "ro=true") {
			t.Fatal("project must remain writable with intact path")
		}
		for _, target := range []string{"/usr", "/opt/node"} {
			if !slices.Contains(mounts[target], "ro=true") {
				t.Errorf("%s must be read-only", target)
			}
		}
		if mode == ModeQwen {
			if !slices.Equal(plan.Workload, []string{"/opt/node/bin/qwen", "--model", "local"}) {
				t.Fatal(plan.Workload)
			}
			if _, present := mounts[sandboxCheckerPath]; present {
				t.Fatal("diagnostic executable leaked into normal sandbox")
			}
		} else if !slices.Contains(mounts[sandboxCheckerPath], "ro=true") {
			t.Fatal("diagnostic executable must be mounted read-only")
		}
	}
}

func TestMinimalRootfs(t *testing.T) {
	directory := t.TempDir()
	if err := writeRunFiles(directory); err != nil {
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
}

func containsSequence(haystack, needle []string) bool {
	for i := range len(haystack) - len(needle) + 1 {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}
