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
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type annotationExpectation struct {
	readOnly, destructive, idempotent, openWorld bool
}

func TestToolCatalogAndIdentity(t *testing.T) {
	session := connect(t)
	initialized := session.InitializeResult()
	if initialized.ServerInfo.Name != "local-runtime-mcp" || initialized.ServerInfo.Version != Version {
		t.Fatalf("unexpected identity: %+v", initialized.ServerInfo)
	}
	if !strings.HasPrefix(initialized.Instructions, "Local Runtime MCP exposes the machine") {
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
		"filesystem_list": {true, false, true, false}, "filesystem_stat": {true, false, true, false},
		"filesystem_read_text": {true, false, true, false}, "filesystem_write_text": {false, true, true, false},
		"filesystem_patch_text": {false, true, true, false}, "filesystem_search_text": {true, false, true, false},
		"image_read": {true, false, true, false}, "process_run": {false, true, false, true},
		"process_continue": {false, true, false, true},
		"browser_status":   {true, false, true, false}, "browser_tabs": {true, false, true, false},
		"browser_open": {false, false, false, true}, "browser_close": {false, true, true, false},
		"browser_navigate": {false, false, false, true}, "browser_snapshot": {true, false, true, true},
		"browser_screenshot": {true, false, true, true}, "browser_action": {false, true, false, true},
		"computer_targets": {true, false, true, false}, "computer_state": {true, false, true, false},
		"computer_action": {false, true, false, false},
	}
	type propertySchema struct {
		Enum []string `json:"enum"`
	}
	schemas := make(map[string]map[string]propertySchema, len(expected))
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
		if registered.InputSchema == nil || registered.OutputSchema == nil {
			t.Fatalf("tool %q has incomplete schemas", registered.Name)
		}
		if got.ReadOnlyHint != want.readOnly || *got.DestructiveHint != want.destructive || got.IdempotentHint != want.idempotent || *got.OpenWorldHint != want.openWorld {
			t.Fatalf("tool %q annotations = %+v, want %+v", registered.Name, got, want)
		}
		data, err := json.Marshal(registered.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]propertySchema `json:"properties"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("decode input schema for %q: %v", registered.Name, err)
		}
		schemas[registered.Name] = schema.Properties
	}
	assertEnum(t, schemas["browser_navigate"]["kind"].Enum, []string{"url", "back", "forward", "reload"})
	assertEnum(t, schemas["browser_action"]["kind"].Enum, []string{"click", "double_click", "hover", "drag", "type_text", "set_value", "press_key", "scroll", "select", "check", "upload_files", "handle_dialog", "evaluate"})
	assertEnum(t, schemas["computer_action"]["kind"].Enum, []string{"activate", "move", "click", "double_click", "drag", "type_text", "set_value", "press_key", "scroll"})
	if _, ok := schemas["filesystem_write_text"]["create_parents"]; !ok {
		t.Fatal("filesystem_write_text is missing explicit create_parents")
	}
	if _, ok := schemas["computer_action"]["element_ref"]; !ok {
		t.Fatal("computer_action is missing UI Automation element_ref")
	}
	for toolName, forbidden := range map[string][]string{
		"computer_state": {"include_accessibility"},
		"browser_action": {"selector"},
	} {
		for _, name := range forbidden {
			if _, ok := schemas[toolName][name]; ok {
				t.Fatalf("tool %q still exposes removed field %q", toolName, name)
			}
		}
	}
}

func assertEnum(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("enum = %q, want %q", got, want)
	}
}

func TestFilesystemImageAndProcessTools(t *testing.T) {
	root := t.TempDir()
	session := connect(t)
	note := filepath.Join(root, "docs", "note.txt")
	write := callOK(t, session, "filesystem_write_text", map[string]any{"path": note, "content": "hello from MCP\n", "create_only": true, "create_parents": true})
	var written filesystem.TextWriteResult
	decodeStructured(t, write, &written)
	if !written.Created || written.Path != note {
		t.Fatalf("unexpected write: %+v", written)
	}
	read := callOK(t, session, "filesystem_read_text", map[string]any{"path": note})
	var readResult filesystem.TextReadResult
	decodeStructured(t, read, &readResult)
	if readResult.Content != "hello from MCP\n" {
		t.Fatalf("unexpected read: %+v", readResult)
	}
	callOK(t, session, "filesystem_patch_text", map[string]any{"path": note, "expected_sha256": written.SHA256, "hunks": []map[string]any{{"old": "hello", "new": "hi", "after": " from MCP"}}})
	callOK(t, session, "filesystem_stat", map[string]any{"path": note})
	callOK(t, session, "filesystem_list", map[string]any{"path": root})
	search := callOK(t, session, "filesystem_search_text", map[string]any{"path": root, "query": "hi"})
	var searched filesystem.SearchResult
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
	imagePath := filepath.Join(root, "figure.png")
	if err := os.WriteFile(imagePath, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	imageResult := callOK(t, session, "image_read", map[string]any{"path": imagePath})
	if len(imageResult.Content) == 0 {
		t.Fatal("image tool returned no content")
	}
	content, ok := imageResult.Content[0].(*mcp.ImageContent)
	if !ok || content.MIMEType != "image/png" || !bytes.Equal(content.Data, encoded.Bytes()) {
		t.Fatalf("unexpected image content: %#v", imageResult.Content[0])
	}

	processResultRaw := callOK(t, session, "process_run", map[string]any{"program": "go", "args": []string{"version"}, "directory": root})
	var processResult runtimeprocess.Result
	decodeStructured(t, processResultRaw, &processResult)
	if processResult.ExitCode != 0 || !strings.Contains(processResult.Stdout, "go version") || processResult.Directory != root {
		t.Fatalf("unexpected process result: %+v", processResult)
	}

	status := callOK(t, session, "browser_status", map[string]any{})
	var browserStatus browser.Status
	decodeStructured(t, status, &browserStatus)
	if browserStatus.Configured {
		t.Fatalf("test bridge should be unconfigured: %+v", browserStatus)
	}
}

func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	bridge, err := browser.Start(ctx, config.Browser{})
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = New(ctx, bridge, nil).Run(ctx, serverTransport) }()
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
