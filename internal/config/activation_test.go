package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOptionalConfigurationIsValidatedWhenActivated(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("http:\n  listen: invalid-address\n  bearer_token: short\nbrowser:\n  listen: 0.0.0.0:9315\n  token: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Resolve(path, Overrides{})
	if err != nil {
		t.Fatal("configuration parsing activated an optional listener")
	}
	if ValidateBrowser(cfg.Browser) == nil {
		t.Fatal("unsafe active browser listener accepted")
	}
	cfg.Browser.Token = ""
	if err := ValidateBrowser(cfg.Browser); err != nil {
		t.Fatal("disabled browser listener rejected")
	}
}
