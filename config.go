package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tools holds verified executable paths. The read-only /usr mount preserves
// these paths inside the sandbox for Qwen and the isolation probe.
type Tools struct {
	Podman, NFT, IP, Node, NPM string
}

func (t *Tools) RunPodman(ctx context.Context, input io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Podman, append([]string{"--remote=false"}, args...)...)
	cmd.Stdin = input
	var output, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podman %s: %w (%s)", args[0], err, strings.TrimSpace(diagnostics.String()))
	}
	return strings.TrimSpace(output.String()), nil
}

// Config holds host paths and tools used to build sandbox mounts and environment.
type Config struct {
	Overrides        modelOverrides // Launcher endpoint overrides; Qwen receives only arguments after --.
	Project          string         // Repository mounted at the same absolute path; root .git is read-only.
	WorkingDirectory string         // Host working directory, a subdirectory of Project.
	// Home is the resolved host home directory; it is not mounted as a whole.
	// Derived paths and settings:
	//   - Home/.local/state/qwen-box -> Home/.qwen [rw]: qwen state shared across qwen-box instances.
	//   - Home/.cache/qwen-box -> Home/.cache [rw]: cache shared across qwen-box instances (GO and NPM caches are also there)
	//   - Home/.qwen/<configs> -> Home/.qwen/<configs> [ro]: qwen configuration
	//   - Home/.qwen/<creds> -> Home/.qwen/<creds> [copy]: qwen initial oauth credentials
	//   - HOME and the generated passwd entry: the same resolved home path.
	Home  string
	Tools Tools // Host executables used to create and supervise the sandbox.
}

type modelOverrides struct {
	Model, AuthType, BaseURL string
}

func (c Config) stateDir() string { return filepath.Join(c.Home, ".local/state/qwen-box") }

func (c Config) cacheDir() string { return filepath.Join(c.Home, ".cache/qwen-box") }

// discoverConfig completes the parsed configuration from the host working
// directory and environment. It rejects root users and resolves symlinks in the
// working and home directories before choosing the nearest ancestor with a .git
// directory or regular file. The search excludes the filesystem root; a symlink
// or other invalid .git marker causes an error. The sandbox working directory
// uses the same resolved absolute path as the host working directory.
//
// It requires executable podman, nft, ip, node, and npm files in /usr/bin, then
// creates shared persistent state at ~/.local/state/qwen-box for all
// launcher executions, regardless of repository. It resolves the state
// path's symlinks and rejects state inside the repository. Home is the resolved
// host home; selected .qwen configuration files are shared read-only over a
// writable launcher-owned .qwen directory backed by Home/.local/state/qwen-box, shared
// across all repositories. Credentials are seeded once from the host and refreshed only
// in that launcher state. The writable cache at ~/.cache/qwen-box is
// mounted as ~/.cache and shared across launcher runs, separately from ordinary
// host caches. New directories use
// mode 0700; existing permissions are unchanged. Directories may remain after
// a later error. Per-launch skeletons are created separately by runSandbox.
//
// The sandbox environment uses fixed defaults, inherits TERM (default
// xterm-256color), and forwards only OPENAI_BASE_URL,
// OPENAI_API_KEY, and OPENAI_MODEL from the remaining host environment, including
// explicitly empty values. Callers must discard the configuration on error.
func discoverConfig(c Config) (Config, error) {
	if os.Geteuid() == 0 {
		return c, fmt.Errorf("run as your ordinary host user, without sudo")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return c, err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return c, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return c, err
	}
	c.Project, err = findProjectRoot(cwd)
	if err != nil {
		return c, err
	}
	c.WorkingDirectory = cwd
	c.Tools, err = discoverTools()
	if err != nil {
		return c, err
	}
	// Share Qwen runtime state across launcher executions and repositories.
	c.Home = home
	if err := os.MkdirAll(c.stateDir(), 0o700); err != nil {
		return c, err
	}
	state, err := filepath.EvalSymlinks(c.stateDir())
	if err != nil {
		return c, err
	}
	if inside(state, c.Project) {
		return c, fmt.Errorf("sandbox state must be outside the project")
	}
	if err := os.MkdirAll(filepath.Join(c.cacheDir(), "npm"), 0o700); err != nil {
		return c, fmt.Errorf("MkdirAll(cache): %w", err)
	}
	return c, nil
}

func discoverTools() (Tools, error) {
	// Our Arch dependencies install these executables at package-owned /usr/bin
	// paths, preserved inside the sandbox by the read-only /usr mount.
	t := Tools{
		Podman: "/usr/bin/podman",
		NFT:    "/usr/bin/nft",
		IP:     "/usr/bin/ip",
		Node:   "/usr/bin/node",
		NPM:    "/usr/bin/npm",
	}
	for _, path := range []string{t.Podman, t.NFT, t.IP, t.Node, t.NPM} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return Tools{}, fmt.Errorf("missing executable %s; see help", path)
		}
	}
	return t, nil
}

func inside(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func findProjectRoot(cwd string) (string, error) {
	for candidate := cwd; candidate != filepath.Dir(candidate); candidate = filepath.Dir(candidate) {
		marker := filepath.Join(candidate, ".git")
		info, err := os.Lstat(marker)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect Git marker: %w", err)
		}
		// Git uses a directory for ordinary repositories and a regular file
		// for linked worktrees and submodules. Never follow a marker symlink.
		if !info.IsDir() && !info.Mode().IsRegular() {
			return "", fmt.Errorf("refusing invalid Git marker %s: it must be a directory or regular file, not a symlink", marker)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("refusing to expose %s: no .git directory or file in this directory or an eligible parent; run inside a Git repository; / cannot be a project root", cwd)
}

// Environment derives the sandbox environment from Home and allowlisted host variables.
func (c Config) Environment() map[string]string {
	term, ok := os.LookupEnv("TERM")
	if !ok {
		term = "xterm-256color"
	}
	cache := filepath.Join(c.Home, ".cache")
	env := map[string]string{
		"HOME":             c.Home,
		"USER":             "qwen",
		"LOGNAME":          "qwen",
		"PATH":             "/usr/bin",
		"LANG":             "C.UTF-8",
		"TERM":             term,
		"TMPDIR":           "/tmp",
		"NPM_CONFIG_CACHE": filepath.Join(cache, "npm"),
		"GOPATH":           filepath.Join(cache, "go"),
	}
	for _, name := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_MODEL"} {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
	return env
}
