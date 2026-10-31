//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
	"github.com/spf13/pflag"
)

// codeError carries the shell exit status for a signal or a terminated workload.
type codeError int

func (e codeError) Error() string { return fmt.Sprintf("exit status %d", e) }

func (e codeError) exitCode() int { return int(e) }

func main() {
	// Strip group/other permissions from newly created files and directories
	// (typically 0600 and 0700), keeping state and credentials private. Child
	// processes, including Qwen, inherit this mask; existing files are unchanged.
	syscall.Umask(0o077)

	signals := make(chan os.Signal, 1)
	// Handle SIGTERM so cancellation can stop and reap children, join scoped
	// tasks, and remove temporary files before exiting. Linux does not normally
	// kill children when a parent exits. Podman's container also needs explicit
	// removal if its attached CLI exits before the workload does.
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	err := scope.Run(context.Background(), func(ctx context.Context, s scope.Scope) error {
		s.SpawnBg(func() error {
			sig, err := utils.Recv(ctx, signals)
			if err != nil {
				return utils.IgnoreCancel(err)
			}
			return codeError(128 + int(sig.(syscall.Signal)))
		})
		args, err := parseArgs(os.Args[1:])
		if err != nil {
			return err
		}
		switch args.Mode {
		case ModeQwen, ModeIsolationCheck:
			return runSandbox(ctx, args)
		case ModeIsolationProbe:
			return runIsolationChecks(ctx, args.Version, args.Arguments[0], os.Stdout)
		case ModeHelp:
			return printHelp(os.Stdout, args.Arguments)
		default:
			return fmt.Errorf("unknown launcher command %q", args.Mode)
		}
	})
	if err != nil {
		slog.Error("sandbox failed", slog.Any("error", err))
		if codeErr, ok := errors.AsType[codeError](err); ok {
			os.Exit(codeErr.exitCode())
		}
		os.Exit(1)
	}
}

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
	flags.Var(&result.Config.Localnet, "localnet", "Comma-separated TCP/UDP IP:port or DNS:port destinations allowed on local networks")
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
  --localnet host:port,...  Allow listed TCP/UDP destinations through local network blocks
  -h, --help              Show launcher help

Use qwen-box help [command] for launcher command help.
Arguments after -- are forwarded to Qwen Code unchanged, including -h/--help.
Only --localnet grants local network exceptions; Qwen settings and arguments do not.

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
including -h and --help. Use --localnet before -- to grant local TCP/UDP access;
Qwen configuration and arguments cannot change the firewall exceptions.
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
