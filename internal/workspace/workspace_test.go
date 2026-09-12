package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hicancan/workspace-mcp/internal/config"
)

func testManager(root string) *Manager {
	return NewManager(&config.Config{Workspaces: map[string]config.Workspace{
		"demo": {Root: root, Description: "test workspace"},
	}})
}

func TestFileLifecycle(t *testing.T) {
	root := t.TempDir()
	m := testManager(root)

	written, err := m.Write("demo", "docs/hello.txt", "Hello Workspace\nsecond line\n")
	if err != nil {
		t.Fatal(err)
	}
	if written.Bytes == 0 || len(written.SHA256) != 64 {
		t.Fatalf("unexpected write result: %+v", written)
	}

	read, err := m.Read("demo", "docs/hello.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if read.Content != "Hello Workspace\nsecond line\n" || read.SHA256 != written.SHA256 {
		t.Fatalf("unexpected read result: %+v", read)
	}

	matches, err := m.Search("demo", "workspace", false, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Line != 1 {
		t.Fatalf("unexpected matches: %+v", matches)
	}

	tree, err := m.Tree("demo", ".", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree[1].Path != "docs/hello.txt" {
		t.Fatalf("unexpected tree: %+v", tree)
	}
	nested, err := m.Tree("demo", "docs", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(nested) != 1 || nested[0].Path != "docs/hello.txt" {
		t.Fatalf("nested tree paths must remain workspace-relative: %+v", nested)
	}
}

func TestExecUsesWorkspaceAsWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	m := testManager(root)
	result, err := m.Exec(context.Background(), "demo", "go", []string{"env", "GOMOD"}, nil, 30)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("go env failed: %+v", result)
	}
	if !strings.Contains(filepath.Clean(result.Stdout), filepath.Clean(root)) && !strings.Contains(result.Stdout, os.DevNull) {
		t.Fatalf("command did not run in workspace: %q", result.Stdout)
	}
}

func TestGitLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	root := filepath.Join(base, "work")
	remote := filepath.Join(base, "remote.git")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	m := testManager(root)
	ctx := context.Background()
	commands := [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Workspace MCP Test"},
		{"config", "user.email", "wmcp-test@example.invalid"},
	}
	for _, args := range commands {
		result, err := m.Git(ctx, "demo", args...)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("git %v failed: err=%v result=%+v", args, err, result)
		}
	}
	if _, err := m.Write("demo", "README.md", "# demo\n"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "initial"}} {
		result, err := m.Git(ctx, "demo", args...)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("git %v failed: err=%v result=%+v", args, err, result)
		}
	}
	if out, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}
	for _, args := range [][]string{{"remote", "add", "origin", remote}, {"push", "-u", "origin", "main"}} {
		result, err := m.Git(ctx, "demo", args...)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("git %v failed: err=%v result=%+v", args, err, result)
		}
	}
	status, err := m.Git(ctx, "demo", "status", "--porcelain=v2", "--branch")
	if err != nil || status.ExitCode != 0 || !strings.Contains(status.Stdout, "# branch.upstream origin/main") {
		t.Fatalf("unexpected status: err=%v result=%+v", err, status)
	}
}
