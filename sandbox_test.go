package main

import (
	"slices"
	"testing"
)

func TestInvocationAndWorkload(t *testing.T) {
	c := Config{Project: "/host/project", Home: "/host/state/home", NodePrefix: "/host/node", WorkingDirectory: "/workspace/sub", Launcher: "/host/launcher", Environment: map[string]string{"HOME": "/home/qwen"}}
	for _, test := range []struct{ args, workload []string }{
		{nil, []string{"/opt/node/bin/qwen"}},
		{[]string{"--model", "a b"}, []string{"/opt/node/bin/qwen", "--model", "a b"}},
		{[]string{"--shell", "-c", "echo hello"}, []string{"/usr/bin/bash", "--noprofile", "--norc", "-c", "echo hello"}},
	} {
		invocation, err := parseInvocation(test.args)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := buildSandbox(c, invocation, "/run-dir")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.Workload, test.workload) {
			t.Errorf("workload: %v", plan.Workload)
		}
		if containsSequence(plan.BubblewrapArguments, []string{"--ro-bind", c.Launcher, sandboxCheckerPath}) {
			t.Fatal("launcher should only be exposed in check mode")
		}
		for _, sequence := range [][]string{
			{"--cap-drop", "ALL", "--clearenv"}, {"--unshare-user", "--disable-userns", "--share-net"},
			{"--bind", "/host/project", "/workspace"}, {"--bind", "/host/state/home", "/home/qwen"},
			{"--ro-bind", "/host/node", "/opt/node"}, {"--chdir", "/workspace/sub"},
		} {
			if !containsSequence(plan.BubblewrapArguments, sequence) {
				t.Errorf("missing isolation arguments %v", sequence)
			}
		}
	}
	if _, err := parseInvocation([]string{"--check", "extra"}); err == nil {
		t.Fatal("check accepted extra arguments")
	}
	invocation, err := parseInvocation([]string{"--check"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildSandbox(c, invocation, "/run-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Workload, []string{sandboxCheckerPath, string(helperCheck), "/run-dir/host-canary"}) {
		t.Fatalf("check should execute the Go helper: %v", plan.Workload)
	}
	if !containsSequence(plan.BubblewrapArguments, []string{"--ro-bind", c.Launcher, sandboxCheckerPath}) {
		t.Fatal("check executable is not mounted read-only")
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
