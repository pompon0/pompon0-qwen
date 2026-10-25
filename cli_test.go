package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestLauncherParsingPreservesForwardedArguments(t *testing.T) {
	for _, test := range []struct {
		args      []string
		mode      Mode
		forwarded []string
	}{
		{mode: ModeQwen},
		{args: []string{"qwen", "--model", "local"}, mode: ModeQwen, forwarded: []string{"--model", "local"}},
		{args: []string{"--model=local", "--new-qwen-option", "value", "--", "isolation-check"}, mode: ModeQwen, forwarded: []string{"--model=local", "--new-qwen-option", "value", "--", "isolation-check"}},
		{args: []string{"--prompt", "isolation-check"}, mode: ModeQwen, forwarded: []string{"--prompt", "isolation-check"}},
		{args: []string{"qwen", "--", "--help", "isolation-check"}, mode: ModeQwen, forwarded: []string{"--", "--help", "isolation-check"}},
		{args: []string{"--help"}, mode: ModeQwen, forwarded: []string{"--help"}},
		{args: []string{"-h"}, mode: ModeQwen, forwarded: []string{"-h"}},
		{args: []string{"isolation-check"}, mode: ModeIsolationCheck},
	} {
		original := slices.Clone(test.args)
		var got Args
		called := false
		command := newCommand(test.args, func(ctx context.Context, args Args) error {
			called = true
			got = args
			return nil
		})
		err := command.ExecuteContext(t.Context())
		if !called {
			t.Fatal("sandbox runner was not called")
		}
		if err != nil || got.Mode != test.mode || !slices.Equal(got.Arguments, test.forwarded) {
			t.Errorf("parse %v: got %v, %v", test.args, got, err)
		}
		if !slices.Equal(original, test.args) {
			t.Fatal("parser modified forwarded input")
		}
	}
}

func TestRejectInvalidLauncherModes(t *testing.T) {
	for _, args := range [][]string{{""}, {"shell"}, {"invalid"}, {"isolation-check", "extra"}, {string(ModeIsolationProbe)}} {
		command := newCommand(args, func(context.Context, Args) error {
			t.Fatal("invalid command reached sandbox runner")
			return nil
		})
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestModelFlagParsing(t *testing.T) {
	got, err := endpointOverrides([]string{"-mlocal", "--unknown-qwen-flag=value", "--auth-type", "openai", "--openai-base-url=http://192.168.1.50:11434/v1"})
	if err != nil || got.Model != "local" || got.AuthType != "openai" || got.BaseURL != "http://192.168.1.50:11434/v1" {
		t.Fatalf("model options: %v, %v", got, err)
	}
	if _, err := endpointOverrides([]string{"--model"}); err == nil {
		t.Fatal("missing model value accepted")
	}
}

func TestLauncherHelpHidesInternalModes(t *testing.T) {
	var output bytes.Buffer
	command := newCommand([]string{"help"}, func(context.Context, Args) error {
		t.Fatal("help reached sandbox runner")
		return nil
	})
	command.SetOut(&output)
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "internal-") || strings.Contains(output.String(), string(ModeIsolationProbe)) {
		t.Fatal("private protocol flags exposed in help")
	}
	for _, name := range []string{"qwen", "isolation-check", "help"} {
		if !strings.Contains(output.String(), name) {
			t.Errorf("help is missing %s", name)
		}
	}
}

func TestCommandPropagatesRunnerError(t *testing.T) {
	want := codeError(23)
	command := newCommand([]string{"qwen"}, func(ctx context.Context, args Args) error {
		if ctx != t.Context() {
			t.Error("runner did not receive command context")
		}
		return want
	})
	if err := command.ExecuteContext(t.Context()); !errors.Is(err, want) {
		t.Fatalf("runner error lost: %v", err)
	}
}

func TestIsolationProbeParsesFlags(t *testing.T) {
	command := newCommand([]string{string(ModeIsolationProbe), "--unknown"}, runSandbox)
	if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("unknown probe flag was not rejected: %v", err)
	}
	command = newCommand([]string{string(ModeIsolationProbe), "--help"}, runSandbox)
	command.SetOut(io.Discard)
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("probe help failed: %v", err)
	}
}
