package cloudflare

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestArgumentsContainOnlyTokenFile(t *testing.T) {
	path := filepath.Join("private", "token")
	want := []string{"tunnel", "--no-autoupdate", "--loglevel", "error", "run", "--token-file", path}
	if got := Arguments(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("Arguments() = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(Arguments(path), " "), "secret-value") {
		t.Fatal("arguments leaked a token value")
	}
}

func TestSanitizedEnvironment(t *testing.T) {
	values := []string{
		"Path=/usr/bin",
		"LOCAL_RUNTIME_MCP_HTTP_BEARER_TOKEN=http-secret",
		"SERVICE_API_KEY=api-secret",
		"ANOTHER_PASSWORD=password-secret",
		"CUSTOM_TUNNEL_SECRET=cloudflare-secret",
	}
	got := sanitizedEnvironment(values, []string{"custom_tunnel_secret"})
	if !reflect.DeepEqual(got, []string{"Path=/usr/bin"}) {
		t.Fatalf("sanitizedEnvironment() = %q", got)
	}
}

func TestPrepareTokenFile(t *testing.T) {
	path, cleanup, err := prepareTokenFile("  secret-value  ")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "secret-value" {
		t.Fatalf("token file = %q", data)
	}
	directory := filepath.Dir(path)
	cleanup()
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("temporary token directory remains: %v", err)
	}
}

func TestFindExplicitBinary(t *testing.T) {
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("placeholder"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := FindBinary(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(path)
	if got != want {
		t.Fatalf("FindBinary() = %q, want %q", got, want)
	}
}
