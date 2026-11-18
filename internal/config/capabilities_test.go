package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapabilityDirectoryUsesConfigurationLocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("capabilities:\n  directory: capabilities\nmodels:\n  key: configured-model-key\n"), 0600); err != nil {
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
	t.Setenv(EnvModelKey, "isolated-child-key")
	cfg, err := Resolve(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.Key != "isolated-child-key" {
		t.Fatal("model key environment override not applied")
	}
}

func TestNativeCapabilityKeyEnvironmentNeedsAnExplicitGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	t.Setenv(EnvNativeCapabilityKey, "isolated-native-key")
	cfg, err := Resolve(path, Overrides{})
	if err != nil || cfg.Capabilities.Native != nil {
		t.Fatal("a credential alone must not enable the native listener", err)
	}
	if err := os.WriteFile(path, []byte("capabilities:\n  native:\n    origin: https://studev.example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Resolve(path, Overrides{})
	if err != nil || cfg.Capabilities.Native == nil || cfg.Capabilities.Native.Key != "isolated-native-key" {
		t.Fatal("explicit native grant did not receive its process key", err)
	}
	stored, err := Load(path)
	if err != nil || stored.Capabilities.Native.Key != "" {
		t.Fatal("process key was persisted to the configuration", err)
	}
}
