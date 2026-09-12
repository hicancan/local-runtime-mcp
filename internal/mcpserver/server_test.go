package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hicancan/workspace-mcp/internal/config"
	"github.com/hicancan/workspace-mcp/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type annotationExpectation struct {
	readOnly    bool
	destructive bool
	idempotent  bool
	openWorld   bool
}

func TestToolCatalogAndAnnotations(t *testing.T) {
	session := connect(t, t.TempDir())
	if resources := session.InitializeResult().Capabilities.Resources; resources != nil {
		t.Fatalf("resources capability must be absent: %+v", resources)
	}
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]annotationExpectation{
		"workspace_list": {true, false, true, false},
		"workspace_info": {true, false, true, false},
		"file_tree":      {true, false, true, false},
		"file_info":      {true, false, true, false},
		"file_read":      {true, false, true, false},
		"file_write":     {false, true, true, false},
		"image_read":     {true, false, true, false},
		"file_search":    {true, false, true, false},
		"git_status":     {true, false, true, false},
		"git_diff":       {true, false, true, false},
		"git_pull":       {false, true, false, false},
		"git_commit":     {false, false, false, false},
		"git_push":       {false, true, false, false},
		"exec":           {false, true, false, true},
	}
	if len(listed.Tools) != len(expected) {
		t.Fatalf("tool count = %d, want %d", len(listed.Tools), len(expected))
	}
	for _, registered := range listed.Tools {
		want, ok := expected[registered.Name]
		if !ok {
			t.Fatalf("unexpected tool %q", registered.Name)
		}
		got := registered.Annotations
		if registered.Title == "" || got == nil || got.DestructiveHint == nil || got.OpenWorldHint == nil {
			t.Fatalf("tool %q has incomplete metadata: %+v", registered.Name, registered)
		}
		if got.ReadOnlyHint != want.readOnly || *got.DestructiveHint != want.destructive || got.IdempotentHint != want.idempotent || *got.OpenWorldHint != want.openWorld {
			t.Fatalf("tool %q annotations = %+v, want %+v", registered.Name, got, want)
		}
	}
}

func TestWorkspaceFileImageAndExecTools(t *testing.T) {
	root := t.TempDir()
	session := connect(t, root)

	callOK(t, session, "workspace_list", map[string]any{})
	callOK(t, session, "workspace_info", map[string]any{"workspace": "demo"})

	writeArgs := map[string]any{"workspace": "demo", "path": "docs/note.txt", "content": "hello from MCP\n"}
	firstWrite := callOK(t, session, "file_write", writeArgs)
	secondWrite := callOK(t, session, "file_write", writeArgs)
	if structuredSHA256(t, firstWrite) != structuredSHA256(t, secondWrite) {
		t.Fatal("file_write is not idempotent for identical input")
	}

	read := callOK(t, session, "file_read", map[string]any{"workspace": "demo", "path": "docs/note.txt"})
	var decoded workspace.ReadResult
	decodeStructured(t, read, &decoded)
	if decoded.Content != "hello from MCP\n" || decoded.SHA256 != structuredSHA256(t, firstWrite) {
		t.Fatalf("unexpected file_read result: %+v", decoded)
	}
	callOK(t, session, "file_info", map[string]any{"workspace": "demo", "path": "docs/note.txt"})
	callOK(t, session, "file_tree", map[string]any{"workspace": "demo", "path": "."})
	search := callOK(t, session, "file_search", map[string]any{"workspace": "demo", "query": "hello"})
	var searchOutput FileSearchOutput
	decodeStructured(t, search, &searchOutput)
	if len(searchOutput.Matches) != 1 {
		t.Fatalf("unexpected file_search result: %+v", searchOutput)
	}

	imagePath := filepath.Join(root, "figure.png")
	imageFile, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	wantImage := image.NewRGBA(image.Rect(0, 0, 3, 2))
	wantImage.Set(1, 1, color.RGBA{R: 255, A: 255})
	if err := png.Encode(imageFile, wantImage); err != nil {
		imageFile.Close()
		t.Fatal(err)
	}
	if err := imageFile.Close(); err != nil {
		t.Fatal(err)
	}
	wantImageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	imageResult := callOK(t, session, "image_read", map[string]any{"workspace": "demo", "path": "figure.png"})
	if len(imageResult.Content) != 1 {
		t.Fatalf("image content count = %d, want 1", len(imageResult.Content))
	}
	imageContent, ok := imageResult.Content[0].(*mcp.ImageContent)
	if !ok || imageContent.MIMEType != "image/png" || !bytes.Equal(imageContent.Data, wantImageBytes) {
		t.Fatalf("unexpected image content: %#v", imageResult.Content[0])
	}

	execResult := callOK(t, session, "exec", map[string]any{
		"workspace": "demo", "command": "go", "args": []string{"env", "GOMOD"},
	})
	var command workspace.CommandResult
	decodeStructured(t, execResult, &command)
	if command.ExitCode != 0 || (!strings.Contains(filepath.Clean(command.Stdout), filepath.Clean(root)) && !strings.Contains(command.Stdout, os.DevNull)) {
		t.Fatalf("unexpected exec result: %+v", command)
	}
}

func TestGitTools(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	root := filepath.Join(base, "work")
	remote := filepath.Join(base, "remote.git")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := workspace.NewManager(&config.Config{Workspaces: map[string]config.Workspace{"demo": {Root: root}}})
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Workspace MCP Test"},
		{"config", "user.email", "wmcp-test@example.invalid"},
	} {
		result, err := manager.Git(ctx, "demo", args...)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("git %v failed: err=%v result=%+v", args, err, result)
		}
	}
	if out, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}
	if result, err := manager.Git(ctx, "demo", "remote", "add", "origin", remote); err != nil || result.ExitCode != 0 {
		t.Fatalf("add remote: err=%v result=%+v", err, result)
	}

	session := connectWithManager(t, manager)
	callOK(t, session, "git_status", map[string]any{"workspace": "demo"})
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callOK(t, session, "git_diff", map[string]any{"workspace": "demo"})
	commit := callOK(t, session, "git_commit", map[string]any{"workspace": "demo", "message": "Initial test commit"})
	var commitResult workspace.CommandResult
	decodeStructured(t, commit, &commitResult)
	if commitResult.ExitCode != 0 {
		t.Fatalf("git_commit failed: %+v", commitResult)
	}
	push := callOK(t, session, "git_push", map[string]any{"workspace": "demo", "branch": "main"})
	var pushResult workspace.CommandResult
	decodeStructured(t, push, &pushResult)
	if pushResult.ExitCode != 0 {
		t.Fatalf("git_push failed: %+v", pushResult)
	}
	pull := callOK(t, session, "git_pull", map[string]any{"workspace": "demo", "branch": "main"})
	var pullResult workspace.CommandResult
	decodeStructured(t, pull, &pullResult)
	if pullResult.ExitCode != 0 {
		t.Fatalf("git_pull failed: %+v", pullResult)
	}
}

func connect(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	manager := workspace.NewManager(&config.Config{Workspaces: map[string]config.Workspace{"demo": {Root: root}}})
	return connectWithManager(t, manager)
}

func connectWithManager(t *testing.T, manager *workspace.Manager) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- New(manager).Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "wmcp-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
	})
	return session
}

func callOK(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil || result.IsError {
		t.Fatalf("%s failed: err=%v result=%+v", name, err, result)
	}
	return result
}

func decodeStructured(t *testing.T, result *mcp.CallToolResult, output any) {
	t.Helper()
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, output); err != nil {
		t.Fatal(err)
	}
}

func structuredSHA256(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	var decoded workspace.WriteResult
	decodeStructured(t, result, &decoded)
	return decoded.SHA256
}
