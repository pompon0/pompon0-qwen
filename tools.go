package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Tools holds verified executable paths. The read-only /usr mount preserves
// these paths inside the sandbox for Qwen and the isolation probe.
type Tools struct {
	Podman, NFT, Node, NPM string
}

// QwenVersion selects a published npm version or dist-tag, never another package
// or a local/Git dependency. npm handles resolving tags and caching installations.
type QwenVersion string

var qwenVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func (v *QwenVersion) Set(value string) error {
	if !qwenVersionPattern.MatchString(value) {
		return fmt.Errorf("invalid Qwen version %q: use an npm version or dist-tag", value)
	}
	*v = QwenVersion(value)
	return nil
}

func (v QwenVersion) String() string {
	if v == "" {
		return "latest"
	}
	return string(v)
}

func (QwenVersion) Type() string { return "version" }

func (t Tools) RunPodman(ctx context.Context, input io.Reader, args ...string) (string, error) {
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

func (t Tools) qwenCommand(version QwenVersion, arguments []string) []string {
	// Keep package discovery outside the project without changing Qwen's working
	// directory. npm installs missing packages in the launcher-only shared cache
	// selected by NPM_CONFIG_CACHE, not the host's ordinary npm cache.
	return append([]string{t.NPM, "exec", "--yes", "--prefer-online",
		"--prefix=/opt/qwen-box-npm",
		"--package=@qwen-code/qwen-code@" + version.String(), "--", "qwen"}, arguments...)
}

// These host-owned inputs are mounted read-only over writable launcher-owned
// Qwen state. Other host .qwen contents, including manual sessions, are not shared.
var qwenSharedConfiguration = [...]string{
	"settings.json", ".env", "QWEN.md", "commands", "skills", "rules",
}

// prepareQwenState initializes the state directory derived from Config.Home,
// shared across launcher executions and
// repositories, separately from the host's normal .qwen. It seeds credentials
// only when absent, preserving tokens refreshed by earlier launcher sessions.
// Root-relative operations keep sandbox-created symlinks from redirecting
// supervisor writes outside launcher state. Missing host configuration is valid;
// the host .qwen directory and credential files must not be symlinks.
func prepareQwenState(home, state string) error {
	private, err := os.OpenRoot(state)
	if err != nil {
		return fmt.Errorf("OpenRoot(Qwen state): %w", err)
	}
	defer private.Close()
	host, err := os.OpenRoot(home)
	if err != nil {
		return fmt.Errorf("OpenRoot(host home): %w", err)
	}
	defer host.Close()
	info, err := host.Lstat(".qwen")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Lstat(host .qwen): %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("host .qwen must be a directory, not a symlink")
	}
	for _, name := range []string{"oauth_creds.json", "mcp-oauth-tokens.json"} {
		if err := seedQwenCredential(host, private, name); err != nil {
			return err
		}
	}
	return nil
}

func seedQwenCredential(host, private *os.Root, name string) error {
	path := filepath.Join(".qwen", name)
	info, err := host.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Lstat(%s): %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("host Qwen credential %s must be a regular file", path)
	}
	source, err := host.Open(path)
	if err != nil {
		return fmt.Errorf("Open(%s): %w", path, err)
	}
	defer source.Close()
	// Publish a complete file without replacing an existing credential. Parallel
	// launcher starts must not see a partially copied token or undo a refresh.
	temporary := ".seed-" + rand.Text()
	destination, err := private.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private credential %s: %w", name, err)
	}
	defer private.Remove(temporary)
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("copy Qwen credential %s: %w", name, err)
	}
	if err := private.Link(temporary, name); err != nil && !os.IsExist(err) {
		return fmt.Errorf("publish Qwen credential %s: %w", name, err)
	}
	return nil
}
