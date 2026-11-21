package logs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindComposePluginFollowsTheCLIOrder(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user")
	system := filepath.Join(root, "system")
	missing := filepath.Join(root, "missing")
	write := func(dir string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "docker-compose")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if got := findComposePlugin([]string{missing}); got != "" {
		t.Errorf("no plugin: got %q", got)
	}
	want := write(system, 0o755)
	if got := findComposePlugin([]string{missing, user, system}); got != want {
		t.Errorf("system plugin: got %q, want %q", got, want)
	}
	// A file the CLI could not run is skipped.
	write(user, 0o644)
	if got := findComposePlugin([]string{user, system}); got != want {
		t.Errorf("non-executable user file: got %q, want %q", got, want)
	}
	userPlugin := write(filepath.Join(root, "user2"), 0o755)
	if got := findComposePlugin([]string{filepath.Dir(userPlugin), system}); got != userPlugin {
		t.Errorf("user plugin first: got %q, want %q", got, userPlugin)
	}
}

func TestFindComposePluginLooksInDockerConfig(t *testing.T) {
	config := t.TempDir()
	dir := filepath.Join(config, "cli-plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "docker-compose")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", config)
	if got := FindComposePlugin(); got != path {
		t.Errorf("got %q, want %q", got, path)
	}
}
