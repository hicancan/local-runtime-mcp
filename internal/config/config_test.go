package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResolvesRelativeRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("roots:\n  demo:\n    path: ./data\n    description: Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Roots["demo"].Path; got != root {
		t.Fatalf("path = %q, want %q", got, root)
	}
}

func TestLoadExpandsEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LRMCP_TEST_ROOT", root)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("roots:\n  demo:\n    path: ${LRMCP_TEST_ROOT}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roots["demo"].Path != root {
		t.Fatalf("path = %q, want %q", cfg.Roots["demo"].Path, root)
	}
}

func TestLoadRejectsUnsetEnvironment(t *testing.T) {
	_ = os.Unsetenv("LRMCP_MISSING_ROOT")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("roots:\n  demo:\n    path: ${LRMCP_MISSING_ROOT}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unset environment variable") {
		t.Fatalf("Load error = %v, want unset environment variable", err)
	}
}

func TestLoadRejectsLegacyWorkspaceConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("workspaces:\n  demo:\n    root: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted removed workspace configuration")
	}
}

func TestLoadBrowserAndComputer(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	t.Setenv("LRMCP_TEST_TOKEN", "0123456789abcdef0123456789abcdef")
	content := "roots:\n  test:\n    path: .\nbrowser:\n  enabled: true\n  listen: 127.0.0.1:9315\n  token: ${LRMCP_TEST_TOKEN}\ncomputer:\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Browser.Enabled || cfg.Browser.Token != os.Getenv("LRMCP_TEST_TOKEN") || !cfg.Computer.Enabled {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRejectsUnsafeBrowserConfig(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	content := "roots:\n  test:\n    path: .\nbrowser:\n  enabled: true\n  listen: 0.0.0.0:9315\n  token: 0123456789abcdef0123456789abcdef\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected non-loopback browser bridge to be rejected")
	}
}

func TestLoadDefaultsToCurrentDirectory(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv(ConfigEnvironment, "")
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
	if got := cfg.Roots["default"].Path; got != root {
		t.Fatalf("path = %q, want %q", got, root)
	}
}

func TestLoadRejectsMissingExplicitConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with a missing explicit configuration")
	}
}
