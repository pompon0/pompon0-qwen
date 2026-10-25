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
	// Helpers run in different privilege contexts and are dispatched before normal
	// CLI setup. They must never load project code, Qwen configuration, or hooks.
	ModeSetup          Mode = "internal-net-setup"
	ModeIsolationProbe Mode = "internal-isolation-probe"
	ModeUserNSProbe    Mode = "internal-userns-probe"
)

type Args struct {
	Mode      Mode
	Arguments []string
}

func newCommand(arguments []string, sandboxRunner func(context.Context, Args) error) *cobra.Command {
	root := &cobra.Command{
		Use:   "qwen-isolated",
		Short: "Run Qwen Code in a rootless Linux sandbox",
		Long: "Run Qwen Code in a rootless Linux sandbox from a Git repository.\n\n" +
			"Qwen is the default command. Requires bubblewrap, nftables,\n" +
			"iproute2, and util-linux. Qwen must be installed in the active Node prefix.\n\n" +
			"Configuration and packaging: https://github.com/pompon0/pompon0-qwen#readme",
		Args:              cobra.NoArgs,
		RunE:              func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.AddCommand(&cobra.Command{
		Use:   string(ModeQwen) + " [arguments...]",
		Short: "Run Qwen Code (default)",
		// Qwen owns --help/-h and all other flags. Its arguments stay unchanged.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandboxRunner(cmd.Context(), Args{Mode: ModeQwen, Arguments: args})
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   string(ModeIsolationCheck),
		Short: "Check filesystem, privileges, and networking isolation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandboxRunner(cmd.Context(), Args{Mode: ModeIsolationCheck})
		},
	})
	// Helpers run before normal CLI setup and never load project code or settings.
	setupCommand := &cobra.Command{
		Use:    string(ModeSetup) + " NFT POLICY SANDBOX [arguments...]",
		Hidden: true,
		Args:   cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			setup := namespaceSetup{
				NFTExecutable:    args[0],
				PolicyFile:       args[1],
				SandboxArguments: args[2:],
			}
			if err := setup.run(createNamespaceTAP); err != nil {
				return fmt.Errorf("namespaceSetup.run(): %w", err)
			}
			return nil
		},
	}
	// After NFT, parsing stops so Bubblewrap owns its flags and -- separator.
	setupCommand.Flags().SetInterspersed(false)
	root.AddCommand(setupCommand)
	root.AddCommand(&cobra.Command{
		Use:    string(ModeIsolationProbe) + " HOST_CANARY",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runIsolationChecks(cmd.Context(), args[0], cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("runIsolationChecks(): %w", err)
			}
			return nil
		},
	})
	root.AddCommand(&cobra.Command{
		Use:    string(ModeUserNSProbe),
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   func(cmd *cobra.Command, args []string) error { return nil },
	})
	// Select the default command before Cobra searches for subcommands, so Qwen
	// flag values that happen to name launcher commands cannot change dispatch.
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		arguments = slices.Concat([]string{string(ModeQwen)}, arguments)
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
