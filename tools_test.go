package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateQwenCredentials(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".qwen"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oauth_creds.json", "mcp-oauth-tokens.json"} {
		if err := os.WriteFile(filepath.Join(home, ".qwen", name), []byte("host token"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareQwenState(home, state); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oauth_creds.json", "mcp-oauth-tokens.json"} {
		path := filepath.Join(state, name)
		if data, err := os.ReadFile(path); err != nil || string(data) != "host token" {
			t.Fatalf("private credentials not seeded: %q, %v", data, err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("credential permissions: %v, %v", info, err)
		}
		if err := os.WriteFile(path, []byte("refreshed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareQwenState(home, state); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oauth_creds.json", "mcp-oauth-tokens.json"} {
		if data, err := os.ReadFile(filepath.Join(state, name)); err != nil || string(data) != "refreshed" {
			t.Fatalf("refreshed credentials overwritten: %q, %v", data, err)
		}
		if data, err := os.ReadFile(filepath.Join(home, ".qwen", name)); err != nil || string(data) != "host token" {
			t.Fatalf("host credentials changed: %q, %v", data, err)
		}
	}
}

func TestPrivateQwenCredentialsDoNotFollowDestinationSymlinks(t *testing.T) {
	home, state, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".qwen"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".qwen/oauth_creds.json"), []byte("host token"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "token")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(state, "oauth_creds.json")); err != nil {
		t.Fatal(err)
	}
	if err := prepareQwenState(home, state); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "untouched" {
		t.Fatalf("credential seeding followed a destination symlink: %q, %v", data, err)
	}
}

func TestPrivateQwenStateWithoutHostConfiguration(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	if err := prepareQwenState(home, state); err != nil {
		t.Fatal(err)
	}
	if err := verifyWritableDirectory(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".qwen")); !os.IsNotExist(err) {
		t.Fatalf("created host configuration directory: %v", err)
	}
}
