package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

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
	Arguments []string
}

func parseArgs(arguments []string) (Args, error) {
	result := Args{Mode: ModeQwen, Version: "latest"}
	flags := pflag.NewFlagSet("qwen-isolated", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&result.Version, "qwen-version", "Qwen npm version or dist-tag")
	help := flags.BoolP("help", "h", false, "Launcher command help")
	// Locate only the launcher's leading version options; pflag parses their
	// values. Qwen owns --help/-h and all other flags in default/Qwen mode.
	modeIndex := 0
	for modeIndex < len(arguments) {
		if arguments[modeIndex] == "--qwen-version" {
			modeIndex = min(modeIndex+2, len(arguments))
		} else if strings.HasPrefix(arguments[modeIndex], "--qwen-version=") {
			modeIndex++
		} else {
			break
		}
	}
	if err := flags.Parse(arguments[:modeIndex]); err != nil {
		return Args{}, fmt.Errorf("parse launcher flags: %w", err)
	}
	remaining := arguments[modeIndex:]
	// Select the mode before inspecting Qwen arguments, so prompt text that
	// happens to name a launcher command cannot change dispatch.
	if len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-") {
		result.Mode = Mode(remaining[0])
		remaining = remaining[1:]
	}
	if result.Mode == ModeQwen {
		result.Arguments = slices.Clone(remaining)
		return result, nil
	}
	if err := flags.Parse(remaining); err != nil {
		return Args{}, fmt.Errorf("parse %s flags: %w", result.Mode, err)
	}
	remaining = flags.Args()
	switch result.Mode {
	case ModeIsolationCheck, ModeIsolationProbe, ModeHelp:
	default:
		return Args{}, fmt.Errorf("unknown launcher command %q", result.Mode)
	}
	if *help {
		result.Arguments = []string{string(result.Mode)}
		result.Mode = ModeHelp
		return result, nil
	}
	switch result.Mode {
	case ModeIsolationCheck:
		if len(remaining) != 0 {
			return Args{}, fmt.Errorf("isolation-check accepts no positional arguments")
		}
	case ModeIsolationProbe:
		if len(remaining) != 1 {
			return Args{}, fmt.Errorf("%s requires HOST_CANARY", result.Mode)
		}
	case ModeHelp:
		if len(remaining) > 1 {
			return Args{}, fmt.Errorf("help accepts at most one command")
		}
		if len(remaining) == 1 {
			switch Mode(remaining[0]) {
			case ModeQwen, ModeIsolationCheck, ModeIsolationProbe, ModeHelp:
			default:
				return Args{}, fmt.Errorf("unknown help command %q", remaining[0])
			}
		}
	}
	result.Arguments = slices.Clone(remaining)
	return result, nil
}

func printHelp(output io.Writer, arguments []string) error {
	text := `Run Qwen Code in a rootless Linux sandbox from a Git repository.

Usage:
  qwen-isolated [--qwen-version VERSION] [qwen] [arguments...]
  qwen-isolated [--qwen-version VERSION] isolation-check
  qwen-isolated help [command]

Commands:
  qwen             Run Qwen Code (default)
  isolation-check  Check filesystem, privileges, and networking isolation
  help             Show launcher help

Options:
  --qwen-version VERSION  Qwen npm version or dist-tag (default latest; before the mode)

Use qwen-isolated help [command] for launcher command help.
In Qwen mode (the default), -h and --help are forwarded to Qwen Code.

Requires Podman, nftables, iproute2, Node.js, and npm.
npm installs and caches the selected Qwen version at runtime inside the sandbox.
Configuration and packaging: https://github.com/pompon0/pompon0-qwen#readme
`
	if len(arguments) == 1 {
		switch Mode(arguments[0]) {
		case ModeQwen:
			text = `Usage: qwen-isolated [--qwen-version VERSION] qwen [arguments...]

Run Qwen Code. All arguments after qwen are forwarded unchanged,
including -h and --help. Put launcher options before qwen.
`
		case ModeIsolationCheck:
			text = `Usage: qwen-isolated [--qwen-version VERSION] isolation-check

Check filesystem, privileges, and networking isolation inside a fresh sandbox.
Use -h or --help after isolation-check to show this help.
`
		case ModeIsolationProbe:
			text = "Usage: qwen-isolated internal-isolation-probe HOST_CANARY\n"
		}
	}
	if _, err := fmt.Fprint(output, text); err != nil {
		return fmt.Errorf("print help: %w", err)
	}
	return nil
}

// Consume values as arguments rather than interpreting prompt text as flags.
// Unknown options stay Qwen's responsibility. A bare -- ends option processing.
func endpointOverrides(arguments []string) (modelOverrides, error) {
	var result modelOverrides
	flags := pflag.NewFlagSet("qwen", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.ParseErrorsAllowlist.UnknownFlags = true
	flags.StringVarP(&result.Model, "model", "m", "", "Model selection")
	flags.StringVar(&result.AuthType, "auth-type", "", "Provider authentication type")
	flags.StringVar(&result.BaseURL, "openai-base-url", "", "Model endpoint")
	// Register opaque string values so their contents cannot become flags.
	var prompt, interactivePrompt, systemPrompt, appendedPrompt, apiKey string
	flags.StringVarP(&prompt, "prompt", "p", "", "Prompt")
	flags.StringVarP(&interactivePrompt, "prompt-interactive", "i", "", "Interactive prompt")
	flags.StringVar(&systemPrompt, "system-prompt", "", "System prompt")
	flags.StringVar(&appendedPrompt, "append-system-prompt", "", "Additional system prompt")
	flags.StringVar(&apiKey, "openai-api-key", "", "API key")
	// Leave Qwen's own help handling to Qwen while still scanning its model flags.
	var help bool
	flags.BoolVarP(&help, "help", "h", false, "Qwen help")
	if err := flags.Parse(slices.Clone(arguments)); err != nil {
		return modelOverrides{}, fmt.Errorf("parse Qwen model flags: %w", err)
	}
	return result, nil
}
