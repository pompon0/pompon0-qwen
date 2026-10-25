package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindProjectRoot(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	child := filepath.Join(project, "nested")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := func(dir string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker(home)
	if err := os.WriteFile(filepath.Join(project, ".qwen-sandbox-root"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := findProjectRoot(child, home); err == nil {
		t.Fatal("accepted home Git marker or obsolete custom marker")
	}
	marker(project)
	if root, err := findProjectRoot(child, home); err != nil || root != project {
		t.Fatalf("got %q, %v", root, err)
	}
	marker(child)
	if root, err := findProjectRoot(child, home); err != nil || root != child {
		t.Fatalf("nearest marker: %q, %v", root, err)
	}
	if err := os.Remove(filepath.Join(child, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(child, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := findProjectRoot(child, home); err == nil {
		t.Fatal("accepted dangling symlink or skipped invalid marker")
	}
}

func TestFindProjectRootWithGitFile(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	child := filepath.Join(project, "nested")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("gitdir: /other/repo/.git/worktrees/project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if root, err := findProjectRoot(child, home); err != nil || root != project {
		t.Fatalf("Git file root: %q, %v", root, err)
	}
}

func TestRejectGitMarkerSymlinkToDirectory(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, filepath.Join(project, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := findProjectRoot(project, home); err == nil {
		t.Fatal("accepted symlink to directory")
	}
}

func TestInsideUsesPathBoundaries(t *testing.T) {
	for _, test := range []struct {
		path, parent string
		want         bool
	}{
		{"/work", "/work", true}, {"/work/sub", "/work", true},
		{"/work-other", "/work", false}, {"/work/../outside", "/work", false},
	} {
		if got := inside(test.path, test.parent); got != test.want {
			t.Errorf("inside(%q,%q) = %v", test.path, test.parent, got)
		}
	}
}

func TestEnvironmentAllowlist(t *testing.T) {
	host := map[string]string{"OPENAI_API_KEY": "", "OPENAI_MODEL": "model", "AWS_SECRET_ACCESS_KEY": "secret", "HOME": "/host", "PATH": "/untrusted", "LD_PRELOAD": "unsafe"}
	env := sandboxEnvironment(func(name string) (string, bool) { value, ok := host[name]; return value, ok })
	if env["HOME"] != "/home/qwen" || env["PATH"] != "/usr/bin" || env["TERM"] != "xterm-256color" {
		t.Fatal("host environment replaced sandbox defaults")
	}
	if value, ok := env["OPENAI_API_KEY"]; !ok || value != "" {
		t.Fatal("explicit empty API key was lost")
	}
	if env["OPENAI_MODEL"] != "model" {
		t.Fatal("model not forwarded")
	}
	for _, key := range []string{"AWS_SECRET_ACCESS_KEY", "LD_PRELOAD", "OPENAI_BASE_URL"} {
		if _, ok := env[key]; ok {
			t.Errorf("unexpected environment key %s", key)
		}
	}
}
