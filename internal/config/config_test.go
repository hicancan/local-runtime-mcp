package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingReturnsDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.Listen != DefaultListen || cfg.Browser.Token != "" {
		t.Fatalf("unexpected default: %+v", cfg)
	}
}

func TestLoadExpandsBrowserToken(t *testing.T) {
	t.Setenv("LRMCP_TEST_TOKEN", "0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("browser:\n  listen: 127.0.0.1:9315\n  token: ${LRMCP_TEST_TOKEN}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.Token != os.Getenv("LRMCP_TEST_TOKEN") {
		t.Fatalf("unexpected token: %q", cfg.Browser.Token)
	}
}

func TestLoadRejectsUnsetEnvironment(t *testing.T) {
	_ = os.Unsetenv("LRMCP_MISSING_TOKEN")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("browser:\n  token: ${LRMCP_MISSING_TOKEN}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unset environment variable") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadRejectsRemovedAndUnsafeConfiguration(t *testing.T) {
	for name, content := range map[string]string{
		"roots":  "roots:\n  demo:\n    path: .\n",
		"legacy": "workspaces:\n  demo:\n    root: .\n",
		"unsafe": "browser:\n  listen: 0.0.0.0:9315\n  token: 0123456789abcdef0123456789abcdef\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted removed or unsafe configuration")
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	want := &Config{Browser: Browser{Listen: "localhost:9315", Token: "0123456789abcdef0123456789abcdef"}}
	resolved, err := Save(path, want)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("resolved path = %q, want %q", resolved, path)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Browser != want.Browser {
		t.Fatalf("round trip = %+v, want %+v", got.Browser, want.Browser)
	}
	want.Browser.Listen = "127.0.0.1:9415"
	if _, err := Save(path, want); err != nil {
		t.Fatalf("replace existing configuration: %v", err)
	}
	got, err = Load(path)
	if err != nil || got.Browser != want.Browser {
		t.Fatalf("replaced round trip = %+v, %v", got.Browser, err)
	}
}
