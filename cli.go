package main

import (
	"fmt"
	"io"
	"slices"

	"github.com/spf13/pflag"
)

type Mode string

const (
	ModeQwen           Mode = "qwen"
	ModeIsolationCheck Mode = "isolation-check"
	ModeHelp           Mode = "help"
	// The probe runs inside the sandbox and bypasses host configuration discovery.
	// It must never load project code, Qwen configuration, or hooks.
	ModeIsolationProbe Mode = "internal-isolation-probe"
)

type Args struct {
	Mode      Mode
	Version   QwenVersion
	Config    Config
	Arguments []string
}

func parseArgs(arguments []string) (Args, error) {
	result := Args{Mode: ModeQwen, Version: "latest"}
	flags := pflag.NewFlagSet("qwen-box", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&result.Version, "qwen-version", "Qwen npm version or dist-tag")
	flags.StringVarP(&result.Config.Overrides.Model, "model", "m", "", "Model selection for endpoint resolution")
	flags.StringVar(&result.Config.Overrides.AuthType, "auth-type", "", "Provider authentication type for endpoint resolution")
	flags.StringVar(&result.Config.Overrides.BaseURL, "openai-base-url", "", "Model endpoint fallback")
	help := flags.BoolP("help", "h", false, "Launcher command help")
	// The first -- separates launcher options from opaque Qwen arguments.
	var forwarded []string
	if separator := slices.Index(arguments, "--"); separator >= 0 {
		forwarded = slices.Clone(arguments[separator+1:])
		arguments = arguments[:separator]
	}
	if err := flags.Parse(arguments); err != nil {
		return Args{}, fmt.Errorf("parse launcher flags: %w", err)
	}
	positionals := flags.Args()
	// Select the mode before inspecting Qwen arguments, so prompt text that
	// happens to name a launcher command cannot change dispatch.
	if len(positionals) > 0 {
		result.Mode = Mode(positionals[0])
		positionals = positionals[1:]
	}

	switch result.Mode {
	case ModeQwen, ModeIsolationCheck, ModeIsolationProbe, ModeHelp:
	default:
		return Args{}, fmt.Errorf("unknown launcher command %q", result.Mode)
	}
	if *help {
		if len(flags.Args()) > 0 {
			result.Arguments = []string{string(result.Mode)}
		}
		result.Mode = ModeHelp
		return result, nil
	}
	if result.Mode != ModeQwen && len(forwarded) > 0 {
		return Args{}, fmt.Errorf("%s accepts no Qwen arguments after --", result.Mode)
	}
	switch result.Mode {
	case ModeQwen:
		if len(positionals) != 0 {
			return Args{}, fmt.Errorf("Qwen arguments must follow --")
		}
		result.Arguments = forwarded
		return result, nil
	case ModeIsolationCheck:
		if len(positionals) != 0 {
			return Args{}, fmt.Errorf("isolation-check accepts no positional arguments")
		}
	case ModeIsolationProbe:
		if len(positionals) != 1 {
			return Args{}, fmt.Errorf("%s requires HOST_CANARY", result.Mode)
		}
	case ModeHelp:
		if len(positionals) > 1 {
			return Args{}, fmt.Errorf("help accepts at most one command")
		}
		if len(positionals) == 1 {
			switch Mode(positionals[0]) {
			case ModeQwen, ModeIsolationCheck, ModeIsolationProbe, ModeHelp:
			default:
				return Args{}, fmt.Errorf("unknown help command %q", positionals[0])
			}
		}
	}
	result.Arguments = slices.Clone(positionals)
	return result, nil
}

func printHelp(output io.Writer, arguments []string) error {
	text := `Run Qwen Code in a rootless Linux sandbox from a Git repository.

Usage:
  qwen-box [options] [qwen] [-- arguments...]
  qwen-box [options] isolation-check
  qwen-box help [command]

Commands:
  qwen             Run Qwen Code (default)
  isolation-check  Check filesystem, privileges, and networking isolation
  help             Show launcher help

Options:
  --qwen-version VERSION  Qwen npm version or dist-tag (default latest)
  -m, --model MODEL       Model selection for endpoint resolution
  --auth-type TYPE        Provider authentication type for endpoint resolution
  --openai-base-url URL   Model endpoint fallback
  -h, --help              Show launcher help

Use qwen-box help [command] for launcher command help.
Arguments after -- are forwarded to Qwen Code unchanged, including -h/--help.
Endpoint overrides affect the launcher only; repeat them after -- for Qwen.

Requires Podman, nftables, iproute2, Node.js, and npm.
npm installs and caches the selected Qwen version at runtime inside the sandbox.
Launcher runs share writable Qwen state and caches, separately from manual runs.
Selected host ~/.qwen configuration entries are mounted read-only.
Configuration and packaging: https://github.com/pompon0/pompon0-qwen#readme
`
	if len(arguments) == 1 {
		switch Mode(arguments[0]) {
		case ModeQwen:
			text = `Usage: qwen-box [options] [qwen] [-- arguments...]

Run Qwen Code. All arguments after -- are forwarded unchanged,
including -h and --help. Endpoint overrides before -- affect only the launcher;
repeat them after -- to pass them to Qwen.
`
		case ModeIsolationCheck:
			text = `Usage: qwen-box [options] isolation-check

Check filesystem, privileges, and networking isolation inside a fresh sandbox.
Use -h or --help after isolation-check to show this help.
`
		case ModeIsolationProbe:
			text = "Usage: qwen-box internal-isolation-probe HOST_CANARY\n"
		}
	}
	if _, err := fmt.Fprint(output, text); err != nil {
		return fmt.Errorf("print help: %w", err)
	}
	return nil
}
