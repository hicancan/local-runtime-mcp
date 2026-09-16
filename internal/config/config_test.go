package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathForExecutable(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "portable", "lrmcp.exe")
	got, err := PathForExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(executable), FileName)
	if got != want {
		t.Fatalf("PathForExecutable() = %q, want %q", got, want)
	}
}

func TestLoadMissingReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.Listen != DefaultBrowser || cfg.HTTP.Listen != DefaultHTTP {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestResolvePrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	content := `browser:
  listen: 127.0.0.1:9401
  token: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
openai:
  tunnel_id: yaml-tunnel
  api_key: yaml-key
http:
  listen: 127.0.0.1:9402
  public_host: yaml.example.com
  bearer_token: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
cloudflare:
  tunnel_token: yaml-cloudflare
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvOpenAITunnelID, "environment-tunnel")
	t.Setenv(EnvHTTPPublicHost, "environment.example.com")
	cfg, err := Resolve(path, Overrides{
		OpenAITunnelID: "flag-tunnel",
		HTTPListen:     "127.0.0.1:9403",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAI.TunnelID != "flag-tunnel" || cfg.HTTP.PublicHost != "environment.example.com" || cfg.Browser.Listen != "127.0.0.1:9401" || cfg.HTTP.Listen != "127.0.0.1:9403" {
		t.Fatalf("unexpected resolved configuration: %+v", cfg)
	}
}

func TestResolveValidatesFinalValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("browser:\n  listen: 0.0.0.0:9315\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBrowserListen, "127.0.0.1:9415")
	cfg, err := Resolve(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.Listen != "127.0.0.1:9415" {
		t.Fatalf("browser listen = %q", cfg.Browser.Listen)
	}
}

func TestLoadRejectsUnknownAndUnsafeConfiguration(t *testing.T) {
	for name, content := range map[string]string{
		"unknown": "workspace: .\n",
		"browser": "browser:\n  listen: 0.0.0.0:9315\n",
		"http":    "http:\n  listen: 0.0.0.0:9316\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted invalid configuration")
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portable", FileName)
	want := &Config{
		Browser: Browser{Listen: "localhost:9315", Token: "0123456789abcdef0123456789abcdef"},
		OpenAI:  OpenAI{TunnelID: "tunnel", APIKey: "key"},
		HTTP: HTTP{
			Listen: "127.0.0.1:9316", PublicHost: "mcp.example.com",
			BearerToken: "abcdef0123456789abcdef0123456789",
		},
		Cloudflare: Cloudflare{TunnelToken: "cloudflare"},
	}
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
	if *got != *want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
