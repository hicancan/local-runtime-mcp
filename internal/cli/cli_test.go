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
	"github.com/hicancan/local-runtime-mcp/internal/version"
)

func TestVersion(t *testing.T) {
	var output, errors bytes.Buffer
	executable := filepath.Join(t.TempDir(), "lrmcp.exe")
	if err := run(context.Background(), []string{"version"}, strings.NewReader(""), &output, &errors, executable); err != nil {
		t.Fatal(err)
	}
	if output.String() != "lrmcp "+version.Version+"\n" {
		t.Fatalf("version output = %q", output.String())
	}
}

func TestHelpDescribesUnifiedCommands(t *testing.T) {
	var output, errors bytes.Buffer
	executable := filepath.Join(t.TempDir(), "lrmcp.exe")
	if err := run(context.Background(), []string{"help"}, strings.NewReader(""), &output, &errors, executable); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"serve --transport stdio", "lrmcp start", "lrmcp stop", "lrmcp status",
		"lrmcp daemon", "lrmcp tunnel up", "lrmcp tunnel down", "lrmcp grant remote",
		"lrmcp ui", "lrmcp audit list", "lrmcp audit search", "lrmcp audit tail", "lrmcp audit export",
		"setup browser", "setup local", "setup client", "setup oauth", "setup remote-token", config.FileName,
		"command line > LOCAL_RUNTIME_MCP_* environment > YAML > defaults",
		"https://github.com/hicancan/local-runtime-mcp", "GNU AGPL v3.0 only",
	} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("help is missing %q: %q", value, output.String())
		}
	}
	for _, legacy := range []string{"connect openai", "expose cloudflare", "browser-setup", "serve --transport local", "serve --transport http", "--model"} {
		if strings.Contains(output.String(), legacy) {
			t.Fatalf("help still contains removed command %q: %q", legacy, output.String())
		}
	}
}

func TestRemovedCommandsAreUnknown(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "lrmcp.exe")
	for _, args := range [][]string{
		nil,
		{"connect", "openai"},
		{"expose", "cloudflare"},
		{"browser-setup"},
		{"tunnel", "openai"},
		{"tunnel", "cloudflare"},
		{"serve", "stdio"},
		{"serve", "--transport", "local"},
		{"serve", "--transport", "http"},
		{"serve", "--stdio"},
		{"serve", "--http"},
	} {
		var output, errors bytes.Buffer
		err := run(context.Background(), args, strings.NewReader(""), &output, &errors, executable)
		if err == nil {
			t.Fatalf("removed command %q was accepted", args)
		}
	}
	// Standard --version / -v / --help / -h aliases are supported alongside version / help.
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		var out, errBuf bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &out, &errBuf, executable); err != nil || !strings.Contains(out.String(), version.Version) {
			t.Fatalf("version alias %q failed: out=%q err=%v", args, out.String(), err)
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		var out, errBuf bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &out, &errBuf, executable); err != nil || out.Len() == 0 {
			t.Fatalf("help alias %q failed: out=%q err=%v", args, out.String(), err)
		}
	}
}

func TestCommandGroupsRejectUnknownMembers(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "lrmcp.exe")
	for _, args := range [][]string{
		{"serve"},
		{"serve", "openai"},
		{"tunnel"},
		{"tunnel", "up"},
		{"tunnel", "down"},
		{"tunnel", "stdio"},
		{"grant"},
		{"grant", "web"},
		{"grant", "remote", "unknown"},
		{"setup"},
		{"setup", "computer"},
	} {
		var output, errors bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &output, &errors, executable); err == nil {
			t.Fatalf("invalid command %q was accepted", args)
		}
	}
}

func TestGrantRemotePersistsOffline(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "lrmcp.exe")
	var out, errBuf bytes.Buffer
	if err := run(context.Background(), []string{"grant", "remote", "openai", "--read-only", "--tool", "filesystem_read_text"}, strings.NewReader(""), &out, &errBuf, executable); err != nil {
		t.Fatalf("grant remote failed: %v (%s)", err, errBuf.String())
	}
	saved, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Grants.Remote.OpenAI.ReadOnly || len(saved.Grants.Remote.OpenAI.Tools) != 1 || saved.Grants.Remote.OpenAI.Tools[0] != "filesystem_read_text" {
		t.Fatalf("unexpected persisted remote grant: %+v", saved.Grants.Remote)
	}
}

func TestBrowserSetupUsesPortablePaths(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "lrmcp.exe")
	directory := filepath.Join(root, "custom-extension")
	var output, errors bytes.Buffer
	args := []string{"setup", "browser", "--directory", directory}
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

func TestSetupRemoteTokenPersistsAndShows(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "lrmcp.exe")
	var out, errBuf bytes.Buffer
	if err := run(context.Background(), []string{"setup", "remote-token", "--id", "astra-1", "--read-only", "--tool", "browser_status"}, strings.NewReader(""), &out, &errBuf, executable); err != nil {
		t.Fatalf("setup remote-token failed: %v (%s)", err, errBuf.String())
	}
	var created struct {
		ID       string   `json:"id"`
		Token    string   `json:"token"`
		ReadOnly bool     `json:"read_only"`
		Tools    []string `json:"tools"`
	}
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "astra-1" || len(created.Token) != 64 || !created.ReadOnly || len(created.Tools) != 1 || created.Tools[0] != "browser_status" {
		t.Fatalf("unexpected remote-token output: %+v", created)
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil || len(cfg.Grants.Remote.Tokens) != 1 || cfg.Grants.Remote.Tokens[0].Token != created.Token {
		t.Fatalf("remote token not persisted cleanly: %+v (%v)", cfg.Grants.Remote.Tokens, err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"setup", "remote-token", "--id", "astra-1", "--show"}, strings.NewReader(""), &out, &errBuf, executable); err != nil {
		t.Fatalf("setup remote-token --show failed: %v", err)
	}
	if !strings.Contains(out.String(), created.Token) {
		t.Fatalf("--show omitted token: %s", out.String())
	}
}
