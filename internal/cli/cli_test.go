package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func TestVersionAndRemovedCapabilityCLI(t *testing.T) {
	var output, errors bytes.Buffer
	if err := Run(context.Background(), []string{"version"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if output.String() != "lrmcp 6.0.1\n" {
		t.Fatalf("version output = %q", output.String())
	}
	if err := Run(context.Background(), []string{"filesystem"}, strings.NewReader(""), &output, &errors); err == nil {
		t.Fatal("removed filesystem CLI was accepted")
	}
}

func TestHelpIncludesSourceAndLicense(t *testing.T) {
	var output, errors bytes.Buffer
	if err := Run(context.Background(), []string{"help"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"https://github.com/hicancan/local-runtime-mcp", "GNU AGPL v3.0 only"} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("help is missing %q: %q", value, output.String())
		}
	}
}

func TestBrowserSetup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "browser-extension")
	configPath := filepath.Join(t.TempDir(), ".lrmcp", "config.yaml")
	var output, errors bytes.Buffer
	args := []string{"browser-setup", "--config", configPath, "--directory", directory}
	if err := Run(context.Background(), args, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "service_worker.js", "runtime_config.js"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Browser.Token) != 64 || cfg.Browser.Listen != config.DefaultListen {
		t.Fatalf("unexpected generated config: %+v", cfg.Browser)
	}
	var result struct {
		Directory string `json:"directory"`
		Config    string `json:"config"`
		Address   string `json:"address"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Directory != directory || result.Config != configPath || result.Address != config.DefaultListen {
		t.Fatalf("unexpected setup output: %+v", result)
	}
	if strings.Contains(output.String(), cfg.Browser.Token) {
		t.Fatal("browser setup leaked token to stdout")
	}
}
