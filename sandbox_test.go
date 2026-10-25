package main

import (
	"slices"
	"testing"
)

func TestInvocationAndWorkload(t *testing.T) {
	c := Config{Project: "/host/project", Home: "/host/state/home", NodePrefix: "/host/node", WorkingDirectory: "/workspace/sub", Environment: map[string]string{"HOME": "/home/qwen"}}
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
	if plan.Workload[len(plan.Workload)-1] != "/run-dir/host-canary" {
		t.Fatal("check canary path missing")
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
