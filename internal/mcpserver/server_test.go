package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type annotationExpectation struct {
	readOnly, destructive, idempotent, openWorld bool
}

func TestToolCatalogAndIdentity(t *testing.T) {
	session := connect(t, t.TempDir())
	initialized := session.InitializeResult()
	if initialized.ServerInfo.Name != "local-runtime-mcp" || initialized.ServerInfo.Version != Version {
		t.Fatalf("unexpected identity: %+v", initialized.ServerInfo)
	}
	if !strings.HasPrefix(initialized.Instructions, "Local Runtime MCP operates on the machine") {
		t.Fatalf("unexpected instructions: %q", initialized.Instructions)
	}
	if initialized.Capabilities.Resources != nil {
		t.Fatalf("resources capability must be absent: %+v", initialized.Capabilities.Resources)
	}
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]annotationExpectation{
		"filesystem_roots": {true, false, true, false}, "filesystem_list": {true, false, true, false},
		"filesystem_stat": {true, false, true, false}, "filesystem_read_text": {true, false, true, false},
		"filesystem_write_text": {false, true, true, false}, "filesystem_edit_text": {false, true, true, false},
		"filesystem_search_text": {true, false, true, false}, "image_read": {true, false, true, false},
		"process_run": {false, true, false, true}, "browser_status": {true, false, true, false},
		"browser_tabs": {true, false, true, false}, "browser_open": {false, false, false, true},
		"browser_close": {false, true, true, false}, "browser_navigate": {false, false, false, true},
		"browser_snapshot": {true, false, true, true}, "browser_screenshot": {true, false, true, true},
		"browser_action": {false, true, false, true}, "computer_screenshot": {true, false, true, false},
		"computer_action": {false, true, false, false},
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

func TestFilesystemImageAndProcessTools(t *testing.T) {
	root := t.TempDir()
	session := connect(t, root)
	callOK(t, session, "filesystem_roots", map[string]any{})
	write := callOK(t, session, "filesystem_write_text", map[string]any{"root": "test", "path": "docs/note.txt", "content": "hello from MCP\n", "create_only": true})
	var written core.TextWriteResult
	decodeStructured(t, write, &written)
	read := callOK(t, session, "filesystem_read_text", map[string]any{"root": "test", "path": "docs/note.txt"})
	var readResult core.TextReadResult
	decodeStructured(t, read, &readResult)
	if readResult.Content != "hello from MCP\n" || readResult.SHA256 != written.SHA256 {
		t.Fatalf("unexpected read: %+v", readResult)
	}
	callOK(t, session, "filesystem_edit_text", map[string]any{"root": "test", "path": "docs/note.txt", "old_text": "hello", "new_text": "hi", "expected_sha256": written.SHA256})
	callOK(t, session, "filesystem_stat", map[string]any{"root": "test", "path": "docs/note.txt"})
	callOK(t, session, "filesystem_list", map[string]any{"root": "test"})
	search := callOK(t, session, "filesystem_search_text", map[string]any{"root": "test", "query": "hi"})
	var searched searchTextOutput
	decodeStructured(t, search, &searched)
	if len(searched.Matches) != 1 {
		t.Fatalf("unexpected search: %+v", searched)
	}

	frame := image.NewRGBA(image.Rect(0, 0, 3, 2))
	frame.Set(1, 1, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "figure.png"), encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	imageResult := callOK(t, session, "image_read", map[string]any{"root": "test", "path": "figure.png"})
	if len(imageResult.Content) == 0 {
		t.Fatal("image tool returned no content")
	}
	content, ok := imageResult.Content[0].(*mcp.ImageContent)
	if !ok || content.MIMEType != "image/png" || !bytes.Equal(content.Data, encoded.Bytes()) {
		t.Fatalf("unexpected image content: %#v", imageResult.Content[0])
	}

	process := callOK(t, session, "process_run", map[string]any{"root": "test", "program": "go", "args": []string{"version"}})
	var processResult core.ProcessResult
	decodeStructured(t, process, &processResult)
	if processResult.ExitCode != 0 || !strings.Contains(processResult.Stdout, "go version") {
		t.Fatalf("unexpected process result: %+v", processResult)
	}

	status := callOK(t, session, "browser_status", map[string]any{})
	var browserStatus browser.Status
	decodeStructured(t, status, &browserStatus)
	if browserStatus.Enabled {
		t.Fatalf("test bridge should be disabled: %+v", browserStatus)
	}
}

func connect(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runtime := core.New(config.Config{Roots: map[string]config.Root{"test": {Path: root}}})
	bridge, err := browser.Start(ctx, config.Browser{})
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = New(runtime, bridge, nil).Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "lrmcp-test", Version: Version}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(); cancel() })
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
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, output); err != nil {
		t.Fatal(err)
	}
}
