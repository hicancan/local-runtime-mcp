package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesRelativeWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("workspaces:\n  demo:\n    root: ./project\n    description: Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "project")
	if got := cfg.Workspaces["demo"].Root; got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}

func TestLoadRejectsEmptyWorkspaceSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("workspaces: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with no workspaces")
	}
}

func TestLoadDefaultsToCurrentDirectory(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("WMCP_CONFIG", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspaces["workspace"].Root; got != root {
		t.Fatalf("root = %q, want %q", got, root)
	}
}

func TestLoadRejectsMissingExplicitConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with a missing explicit configuration")
	}
}
