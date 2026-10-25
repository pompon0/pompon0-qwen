package main

import (
	"bytes"
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
		got, err := parseArgs(test.args)
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
		if _, err := parseArgs(args); err == nil {
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

func TestLauncherHelp(t *testing.T) {
	var output bytes.Buffer
	args, err := parseArgs([]string{"help"})
	if err != nil || args.Mode != ModeHelp {
		t.Fatalf("help dispatch: %+v, %v", args, err)
	}
	if err := printHelp(&output, args.Arguments); err != nil {
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
	for _, misleading := range []string{"help for qwen-isolated", "[command] --help"} {
		if strings.Contains(output.String(), misleading) {
			t.Errorf("misleading launcher help: %s", output.String())
		}
	}
	for _, instruction := range []string{"qwen-isolated help", "forwarded to Qwen Code", "help [command]"} {
		if !strings.Contains(output.String(), instruction) {
			t.Errorf("help is missing %q: %s", instruction, output.String())
		}
	}
}

func TestQwenVersionSelection(t *testing.T) {
	for _, test := range []struct {
		arguments []string
		version   QwenVersion
		mode      Mode
		forwarded []string
	}{
		{version: "latest", mode: ModeQwen},
		{arguments: []string{"--qwen-version", "0.26.1"}, version: "0.26.1", mode: ModeQwen},
		{arguments: []string{"--qwen-version=nightly", "--model", "local"}, version: "nightly", mode: ModeQwen, forwarded: []string{"--model", "local"}},
		{arguments: []string{"--qwen-version", "0.26.1", "qwen", "--prompt", "--qwen-version=other"}, version: "0.26.1", mode: ModeQwen, forwarded: []string{"--prompt", "--qwen-version=other"}},
		{arguments: []string{"--qwen-version", "0.26.1", "isolation-check"}, version: "0.26.1", mode: ModeIsolationCheck},
	} {
		args, err := parseArgs(test.arguments)
		if err != nil {
			t.Fatalf("arguments %v: %v", test.arguments, err)
		}
		if args.Version != test.version || args.Mode != test.mode || !slices.Equal(args.Arguments, test.forwarded) {
			t.Errorf("arguments %v: got %+v", test.arguments, args)
		}
	}
}

func TestRejectInvalidQwenVersions(t *testing.T) {
	for _, arguments := range [][]string{
		{"--qwen-version"}, {"--qwen-version="},
		{"--qwen-version=file:/tmp/package", "qwen"},
		{"--qwen-version=npm:other-package", "qwen"},
		{"--qwen-version=../package", "qwen"},
	} {
		if _, err := parseArgs(arguments); err == nil {
			t.Errorf("accepted invalid version arguments %v", arguments)
		}
	}
}

func TestIsolationProbeParsesFlags(t *testing.T) {
	if _, err := parseArgs([]string{string(ModeIsolationProbe), "--unknown"}); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("unknown probe flag was not rejected: %v", err)
	}
	args, err := parseArgs([]string{string(ModeIsolationProbe), "--help"})
	if err != nil || args.Mode != ModeHelp || !slices.Equal(args.Arguments, []string{string(ModeIsolationProbe)}) {
		t.Fatalf("probe help failed: %+v, %v", args, err)
	}
	args, err = parseArgs([]string{string(ModeIsolationProbe), "--qwen-version", "0.26.1", "/host/canary"})
	if err != nil || args.Mode != ModeIsolationProbe || args.Version != "0.26.1" || !slices.Equal(args.Arguments, []string{"/host/canary"}) {
		t.Fatalf("probe dispatch: %+v, %v", args, err)
	}
}
