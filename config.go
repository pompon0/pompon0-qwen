package main

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/pompon0/pompon0-qwen/utils"
)

// Tools are resolved on the host before entering any namespace.
type Tools struct {
	Podman, NFT, IP string
}

type Config struct {
	Project, WorkingDirectory, State, Home string
	Launcher                               string
	Tools                                  Tools
	ModelServer                            utils.Option[ModelServer]
	ExtraDenied                            []netip.Prefix
	Environment                            map[string]string
}

func discoverConfig() (Config, error) {
	var c Config
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
	c.Project, err = findProjectRoot(cwd, home)
	if err != nil {
		return c, err
	}
	relative, err := filepath.Rel(c.Project, cwd)
	if err != nil {
		return c, err
	}
	c.WorkingDirectory = filepath.Join("/workspace", relative)
	c.Tools, err = discoverTools()
	if err != nil {
		return c, err
	}
	// Retain the legacy state path so existing Qwen settings survive migrations.
	c.State = filepath.Join(home, ".local/state/qwen-bwrap", fmt.Sprintf("%x", sha256.Sum256([]byte(c.Project))))
	if err := os.MkdirAll(c.State, 0o700); err != nil {
		return c, err
	}
	c.State, err = filepath.EvalSymlinks(c.State)
	if err != nil {
		return c, err
	}
	c.Launcher, err = os.Executable()
	if err != nil {
		return c, err
	}
	c.Launcher, err = filepath.EvalSymlinks(c.Launcher)
	if err != nil {
		return c, err
	}
	if inside(c.State, c.Project) {
		return c, fmt.Errorf("sandbox state must be outside the project")
	}
	if inside(c.Launcher, c.Project) || inside(c.Launcher, c.State) {
		return c, fmt.Errorf("install this launcher outside the project and sandbox state")
	}
	c.Home = filepath.Join(c.State, "home")
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return c, err
	}
	for value := range strings.FieldsSeq(strings.ReplaceAll(os.Getenv("QWEN_ISOLATED_DENY"), ",", " ")) {
		prefix, err := parseIPv4Prefix(value)
		if err != nil {
			return c, fmt.Errorf("QWEN_ISOLATED_DENY: %w", err)
		}
		c.ExtraDenied = append(c.ExtraDenied, prefix)
	}
	c.Environment = sandboxEnvironment(os.LookupEnv)
	return c, nil
}

func discoverTools() (Tools, error) {
	t := Tools{
		Podman: "/usr/bin/podman",
		NFT:    "/usr/bin/nft",
		IP:     "/usr/bin/ip",
	}
	for _, path := range []string{t.Podman, t.NFT, t.IP, "/usr/bin/node", "/usr/bin/npm"} {
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

func findProjectRoot(cwd, home string) (string, error) {
	for candidate := cwd; candidate != home && candidate != filepath.Dir(candidate); candidate = filepath.Dir(candidate) {
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
	return "", fmt.Errorf("refusing to expose %s: no .git directory or file in this directory or an eligible parent; run inside a Git repository; home and / cannot be project roots", cwd)
}

func sandboxEnvironment(lookup func(string) (string, bool)) map[string]string {
	term, ok := lookup("TERM")
	if !ok {
		term = "xterm-256color"
	}
	env := map[string]string{
		"HOME": "/home/qwen", "USER": "qwen", "LOGNAME": "qwen",
		"PATH": "/usr/bin", "LANG": "C.UTF-8", "TERM": term, "TMPDIR": "/tmp",
		"NPM_CONFIG_CACHE": "/home/qwen/.cache/npm",
		"XDG_CONFIG_HOME":  "/home/qwen/.config", "XDG_CACHE_HOME": "/home/qwen/.cache",
		"XDG_DATA_HOME": "/home/qwen/.local/share", "XDG_STATE_HOME": "/home/qwen/.local/state",
	}
	for _, name := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_MODEL"} {
		if value, ok := lookup(name); ok {
			env[name] = value
		}
	}
	return env
}
