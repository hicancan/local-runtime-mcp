package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapabilityDirectoryUsesConfigurationLocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("capabilities:\n  directory: capabilities\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stored, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Capabilities.Directory != "capabilities" {
		t.Fatal("loading persistent settings destroyed a portable relative path")
	}
	resolved, err := Resolve(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Capabilities.Directory != filepath.Join(dir, "capabilities") {
		t.Fatalf("directory depends on cwd: %q", resolved.Capabilities.Directory)
	}
}

func TestClientTokenEnvironmentNeedsAnExplicitGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	t.Setenv(EnvClientToken, "isolated-client-token-0123456789abcdef")
	cfg, err := Resolve(path, Overrides{})
	if err != nil || len(cfg.Grants.Clients) != 0 {
		t.Fatal("a credential alone must not enable a local client grant", err)
	}
	if err := os.WriteFile(path, []byte("grants:\n  clients:\n    - id: studev-win\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Resolve(path, Overrides{})
	if err != nil || len(cfg.Grants.Clients) != 1 || cfg.Grants.Clients[0].Token != "isolated-client-token-0123456789abcdef" {
		t.Fatalf("explicit single client grant did not receive EnvClientToken: %+v, %v", cfg.Grants.Clients, err)
	}
	t.Setenv(ClientTokenEnvVar("studev-win"), "per-client-token-0123456789abcdefghijk")
	cfg, err = Resolve(path, Overrides{})
	if err != nil || cfg.Grants.Clients[0].Token != "per-client-token-0123456789abcdefghijk" {
		t.Fatalf("per-client env var did not override generic client token: %+v, %v", cfg.Grants.Clients, err)
	}
	stored, err := Load(path)
	if err != nil || stored.Grants.Clients[0].Token != "" {
		t.Fatal("process key was persisted to the configuration")
	}
}

func TestLegacyFieldsAreRejected(t *testing.T) {
	for name, content := range map[string]string{
		"models.key":          "models:\n  key: old-model-key\n",
		"capabilities.web":    "capabilities:\n  web:\n    - site: https://example.com\n",
		"capabilities.native": "capabilities:\n  native:\n    site: https://example.com\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("removed legacy field %s was accepted", name)
			}
		})
	}
}
