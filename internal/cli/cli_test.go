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

func TestVersion(t *testing.T) {
	var output, errors bytes.Buffer
	executable := filepath.Join(t.TempDir(), "lrmcp.exe")
	if err := run(context.Background(), []string{"version"}, strings.NewReader(""), &output, &errors, executable); err != nil {
		t.Fatal(err)
	}
	if output.String() != "lrmcp 8.0.0\n" {
		t.Fatalf("version output = %q", output.String())
	}
}

func TestHelpDescribesConfiguration(t *testing.T) {
	var output, errors bytes.Buffer
	executable := filepath.Join(t.TempDir(), "lrmcp.exe")
	if err := run(context.Background(), []string{"help"}, strings.NewReader(""), &output, &errors, executable); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"connect openai", "serve http", "expose cloudflare", config.FileName,
		"command line > LOCAL_RUNTIME_MCP_* environment > YAML > defaults",
		"https://github.com/hicancan/local-runtime-mcp", "GNU AGPL v3.0 only",
	} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("help is missing %q: %q", value, output.String())
		}
	}
}

func TestBrowserSetupUsesPortablePaths(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "lrmcp.exe")
	directory := filepath.Join(root, "custom-extension")
	var output, errors bytes.Buffer
	args := []string{"browser-setup", "--directory", directory}
	if err := run(context.Background(), args, strings.NewReader(""), &output, &errors, executable); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "service_worker.js", "runtime_config.js"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, config.FileName)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Browser.Token) != 64 || cfg.Browser.Listen != config.DefaultBrowser {
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
	if result.Directory != directory || result.Config != configPath || result.Address != config.DefaultBrowser {
		t.Fatalf("unexpected setup output: %+v", result)
	}
	if strings.Contains(output.String(), cfg.Browser.Token) {
		t.Fatal("browser setup leaked token to stdout")
	}
}
