package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type Mode string

const (
	ModeQwen           Mode = "qwen"
	ModeIsolationCheck Mode = "isolation-check"
	// The probe runs inside the sandbox and bypasses host configuration discovery.
	// It must never load project code, Qwen configuration, or hooks.
	ModeIsolationProbe Mode = "internal-isolation-probe"
)

type Args struct {
	Mode      Mode
	Version   QwenVersion
	Arguments []string
}

func newCommand(arguments []string, sandboxRunner func(context.Context, Args) error) *cobra.Command {
	version := QwenVersion("latest")
	root := &cobra.Command{
		Use:   "qwen-isolated",
		Short: "Run Qwen Code in a rootless Linux sandbox",
		Long: "Run Qwen Code in a rootless Linux sandbox from a Git repository.\n\n" +
			"Qwen is the default command. Requires Podman, nftables,\n" +
			"iproute2, Node.js, and npm. npm installs the selected Qwen version at runtime.\n\n" +
			"Configuration and packaging: https://github.com/pompon0/pompon0-qwen#readme",
		Args:              cobra.NoArgs,
		RunE:              func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		SilenceErrors:     true,
		SilenceUsage:      true,
		TraverseChildren:  true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.PersistentFlags().Var(&version, "qwen-version", "Qwen npm version or dist-tag (before the mode)")
	root.AddCommand(&cobra.Command{
		Use:   string(ModeQwen) + " [arguments...]",
		Short: "Run Qwen Code (default)",
		// Qwen owns --help/-h and all other flags. Its arguments stay unchanged.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandboxRunner(cmd.Context(), Args{Mode: ModeQwen, Version: version, Arguments: args})
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   string(ModeIsolationCheck),
		Short: "Check filesystem, privileges, and networking isolation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandboxRunner(cmd.Context(), Args{Mode: ModeIsolationCheck, Version: version})
		},
	})
	root.AddCommand(&cobra.Command{
		Use:    string(ModeIsolationProbe) + " HOST_CANARY",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runIsolationChecks(cmd.Context(), version, args[0], cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("runIsolationChecks(): %w", err)
			}
			return nil
		},
	})
	// Select the default command before Cobra searches for subcommands, so Qwen
	// flag values that happen to name launcher commands cannot change dispatch.
	// Skip only the launcher's leading version option when locating the mode;
	// Cobra still parses and validates it. Everything after qwen belongs to Qwen,
	// including prompt text that resembles launcher flags or commands.
	modeIndex := 0
	for modeIndex < len(arguments) {
		if arguments[modeIndex] == "--qwen-version" {
			if modeIndex+1 == len(arguments) {
				root.SetArgs(arguments) // Let Cobra report the missing value.
				return root
			}
			modeIndex += 2
		} else if strings.HasPrefix(arguments[modeIndex], "--qwen-version=") {
			modeIndex++
		} else {
			break
		}
	}
	if modeIndex == len(arguments) || strings.HasPrefix(arguments[modeIndex], "-") {
		arguments = slices.Concat(arguments[:modeIndex], []string{string(ModeQwen)}, arguments[modeIndex:])
	}
	root.SetArgs(arguments)
	return root
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
