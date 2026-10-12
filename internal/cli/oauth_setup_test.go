package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func TestOAuthSetupPreservesCredentialsAndRegistersExactCallbacks(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "lrmcp.exe")
	path := filepath.Join(root, config.FileName)
	cfg := config.Default()
	cfg.HTTP.PublicHost = "mcp.example.com"
	cfg.Browser.Token = strings.Repeat("b", 32)
	cfg.OpenAI.APIKey = "configured-provider-key"
	if _, err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) map[string]any {
		t.Helper()
		var out, diagnostics bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &out, &diagnostics, executable); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(out.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := invoke("setup", "oauth", "--redirect-uri", "https://client.example.com/callback")
	if first["client_id"] != "gemini" || len(first["client_secret"].(string)) < 32 || first["owner_token"] == first["client_secret"] {
		t.Fatal("setup produced invalid credentials")
	}
	second := invoke("setup", "oauth", "--redirect-uri", "https://client.example.com/callback", "--redirect-uri", "https://second.example.com/callback")
	if first["client_secret"] != second["client_secret"] || first["owner_token"] != second["owner_token"] {
		t.Fatal("registration silently rotated credentials")
	}
	stored, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.HTTP.Auth != "oauth" || len(stored.HTTP.OAuth.Clients) != 1 || len(stored.HTTP.OAuth.Clients[0].RedirectURIs) != 2 || stored.Browser.Token != cfg.Browser.Token || stored.OpenAI.APIKey != cfg.OpenAI.APIKey {
		t.Fatal("registration changed unrelated configuration")
	}
	before, _ := os.ReadFile(path)
	shown := invoke("setup", "oauth", "--show")
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !reflect.DeepEqual(shown, second) {
		t.Fatal("show mutated configuration or credentials")
	}
	var out, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"setup", "oauth", "--redirect-uri", "http://untrusted.example.com/callback"}, strings.NewReader(""), &out, &diagnostics, executable); err == nil {
		t.Fatal("unsafe redirect accepted")
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed registration wrote configuration")
	}
}

func TestStdioIgnoresInactiveHTTPConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, config.FileName)
	if err := os.WriteFile(path, []byte("http:\n  listen: invalid-http-address\n  auth: oauth\n  bearer_token: short\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"serve", "--transport", "stdio"}, strings.NewReader(""), &out, &diagnostics, filepath.Join(root, "lrmcp.exe")); err != nil {
		t.Fatalf("inactive HTTP blocked stdio: %v", err)
	}
}
