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
		{args: []string{"qwen"}, mode: ModeQwen},
		{args: []string{"qwen", "--", "--model", "local"}, mode: ModeQwen, forwarded: []string{"--model", "local"}},
		{args: []string{"--", "--model=local", "--new-qwen-option", "value", "--", "health-check"}, mode: ModeQwen, forwarded: []string{"--model=local", "--new-qwen-option", "value", "--", "health-check"}},
		{args: []string{"--", "--prompt", "health-check"}, mode: ModeQwen, forwarded: []string{"--prompt", "health-check"}},
		{args: []string{"qwen", "--", "--", "--help", "health-check"}, mode: ModeQwen, forwarded: []string{"--", "--help", "health-check"}},
		{args: []string{"--", "--help"}, mode: ModeQwen, forwarded: []string{"--help"}},
		{args: []string{"--", "-h"}, mode: ModeQwen, forwarded: []string{"-h"}},
		{args: []string{"--", "health-check"}, mode: ModeQwen, forwarded: []string{"health-check"}},
		{args: []string{"--help"}, mode: ModeHelp},
		{args: []string{"-h"}, mode: ModeHelp},
		{args: []string{"health-check"}, mode: ModeHealthCheck},
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
	for _, args := range [][]string{{""}, {"shell"}, {"invalid"}, {"health-check", "extra"}, {string(ModeHealthCheckProbe)}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestLauncherRejectsEndpointFlags(t *testing.T) {
	for _, mode := range [][]string{{}, {"qwen"}, {"health-check"}} {
		for _, flag := range []string{"--model=local", "-mlocal", "--auth-type=openai", "--openai-base-url=http://192.168.1.50:11434/v1"} {
			arguments := append(slices.Clone(mode), flag)
			if _, err := parseArgs(arguments); err == nil {
				t.Errorf("accepted removed endpoint flag: %v", arguments)
			}
		}
	}
	forwarded := []string{"--model=other", "--auth-type=anthropic", "--openai-base-url=other", "--qwen-version=other"}
	got, err := parseArgs(append([]string{"--"}, forwarded...))
	if err != nil || !slices.Equal(got.Arguments, forwarded) || got.Version != "latest" {
		t.Fatalf("Qwen arguments affected launcher options: %+v, %v", got, err)
	}
	for _, arguments := range [][]string{
		{"--model"}, {"--auth-type"}, {"--openai-base-url"},
		{"--unknown-qwen-flag=value"}, {"--prompt", "hello"},
		{"qwen", "prompt"}, {"health-check", "--", "--model=other"},
	} {
		if _, err := parseArgs(arguments); err == nil {
			t.Errorf("accepted invalid launcher arguments %v", arguments)
		}
	}
}

func TestLocalnetFlag(t *testing.T) {
	for _, arguments := range [][]string{
		{"--localnet=192.168.1.50:11434,[fd12::50]:8000"},
		{"qwen", "--localnet", "192.168.1.50:11434,[fd12::50]:8000"},
		{"--localnet=192.168.1.50:11434,[fd12::50]:8000", "health-check"},
		{"--localnet", " [::ffff:192.168.1.50]:11434,192.168.1.50:11434,[fd12::50]:8000 "},
	} {
		got, err := parseArgs(arguments)
		if err != nil || got.Localnet.String() != "192.168.1.50:11434,[fd12::50]:8000" {
			t.Fatalf("localnet %v: %+v, %v", arguments, got, err)
		}
	}
	got, err := parseArgs([]string{"--localnet=Model.TEST:8000,192.168.1.50:11434"})
	if err != nil || got.Localnet.String() != "192.168.1.50:11434,model.test:8000" {
		t.Fatalf("DNS localnet entry: %+v, %v", got, err)
	}
	for _, arguments := range [][]string{{}, {"--localnet="}, {"--", "--localnet=192.168.1.50:11434"}} {
		got, err := parseArgs(arguments)
		if err != nil || len(got.Localnet) != 0 {
			t.Fatalf("unexpected localnet grant for %v: %+v, %v", arguments, got, err)
		}
	}
	for _, value := range []string{"192.168.1.50", "192.168.1.0/24", "http://192.168.1.50:8000", "192.168.1.50:0", "192.168.1.50:65536", "[fe80::1%eth0]:8000", "192.168.1.50:8000,", ",", " ", ":8000", "model.test:http"} {
		if _, err := parseArgs([]string{"--localnet", value}); err == nil {
			t.Errorf("accepted invalid localnet %q", value)
		}
	}
	if _, err := parseArgs([]string{"--localnet"}); err == nil {
		t.Fatal("accepted localnet without a value")
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
	if strings.Contains(output.String(), "internal-") || strings.Contains(output.String(), string(ModeHealthCheckProbe)) {
		t.Fatal("private protocol flags exposed in help")
	}
	for _, name := range []string{"qwen", "health-check", "help", "--qwen-version", "--localnet"} {
		if !strings.Contains(output.String(), name) {
			t.Errorf("help is missing %s", name)
		}
	}
	for _, removed := range []string{"--model", "--auth-type", "--openai-base-url"} {
		if strings.Contains(output.String(), removed) {
			t.Errorf("help advertises removed launcher flag %s", removed)
		}
	}
	for _, misleading := range []string{"help for qwen-box", "[command] --help"} {
		if strings.Contains(output.String(), misleading) {
			t.Errorf("misleading launcher help: %s", output.String())
		}
	}
	for _, instruction := range []string{"qwen-box help", "forwarded to Qwen Code unchanged", "help [command]"} {
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
		{arguments: []string{"--qwen-version=nightly", "--", "--model", "local"}, version: "nightly", mode: ModeQwen, forwarded: []string{"--model", "local"}},
		{arguments: []string{"--qwen-version", "0.26.1", "qwen", "--", "--prompt", "--qwen-version=other"}, version: "0.26.1", mode: ModeQwen, forwarded: []string{"--prompt", "--qwen-version=other"}},
		{arguments: []string{"--qwen-version", "0.26.1", "health-check"}, version: "0.26.1", mode: ModeHealthCheck},
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

func TestHealthCheckProbeParsesFlags(t *testing.T) {
	if _, err := parseArgs([]string{string(ModeHealthCheckProbe), "--unknown"}); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("unknown probe flag was not rejected: %v", err)
	}
	args, err := parseArgs([]string{string(ModeHealthCheckProbe), "--help"})
	if err != nil || args.Mode != ModeHelp || !slices.Equal(args.Arguments, []string{string(ModeHealthCheckProbe)}) {
		t.Fatalf("probe help failed: %+v, %v", args, err)
	}
	args, err = parseArgs([]string{string(ModeHealthCheckProbe), "--qwen-version", "0.26.1", "/host/canary"})
	if err != nil || args.Mode != ModeHealthCheckProbe || args.Version != "0.26.1" || !slices.Equal(args.Arguments, []string{"/host/canary"}) {
		t.Fatalf("probe dispatch: %+v, %v", args, err)
	}
}

func TestFirewallSetupParsing(t *testing.T) {
	args, err := parseArgs([]string{string(ModeFirewallSetup)})
	if err != nil || args.Mode != ModeFirewallSetup {
		t.Fatalf("firewall setup dispatch: %+v, %v", args, err)
	}
	for _, arguments := range [][]string{
		{string(ModeFirewallSetup), "extra"},
		{string(ModeFirewallSetup), "--", "extra"},
	} {
		if _, err := parseArgs(arguments); err == nil {
			t.Fatalf("accepted invalid setup arguments: %v", arguments)
		}
	}
}
