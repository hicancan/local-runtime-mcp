package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/core"
)

func TestVersionAndFilesystemCLI(t *testing.T) {
	var output, errors bytes.Buffer
	if err := Run(context.Background(), []string{"version"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if output.String() != "lrmcp 2.0.0\n" {
		t.Fatalf("version output = %q", output.String())
	}

	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configuration := "roots:\n  test:\n    path: " + strings.ReplaceAll(root, "\\", "/") + "\n"
	if err := os.WriteFile(configPath, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	args := []string{"filesystem", "write", "--config", configPath, "--root", "test", "--path", "note.txt", "--content", "hello", "--create-only"}
	if err := Run(context.Background(), args, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	var written core.TextWriteResult
	if err := json.Unmarshal(output.Bytes(), &written); err != nil {
		t.Fatal(err)
	}
	if !written.Created || written.Bytes != 5 {
		t.Fatalf("unexpected CLI output: %+v", written)
	}
	data, err := os.ReadFile(filepath.Join(root, "note.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("written file = %q, err=%v", data, err)
	}
}

func TestBrowserExtensionCLI(t *testing.T) {
	t.Setenv("LRMCP_CONFIG", "")
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("HOME", os.Getenv("USERPROFILE"))
	directory := filepath.Join(t.TempDir(), "browser-extension")
	var output, errors bytes.Buffer
	if err := Run(context.Background(), []string{"browser", "extension", "--directory", directory}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"browser", "token"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(output.String())) != 64 {
		t.Fatalf("token length = %d", len(strings.TrimSpace(output.String())))
	}
}
