package runtimehost

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Release smoke test: opt in with a built or installed executable, rather than
// an in-memory server. Browser/desktop actions have separate owned-fixture E2E
// tests; this test does not send input to the user's applications.
func TestExecutableStdio(t *testing.T) {
	executable := os.Getenv("LOCAL_RUNTIME_MCP_EXECUTABLE")
	if executable == "" {
		t.Skip("set LOCAL_RUNTIME_MCP_EXECUTABLE to a release executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.Command(executable, "serve", "stdio", "--browser-listen", "127.0.0.1:0", "--browser-token", "release-smoke-test-credential-0001")
	command.Dir = t.TempDir()
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), config.EnvironmentPrefix) {
			command.Env = append(command.Env, entry)
		}
	}
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	session, err := mcp.NewClient(&mcp.Implementation{Name: "release-smoke", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{
		Command: command, TerminateDuration: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("connect executable: %v; diagnostics: %s", err, diagnostics.String())
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close executable: %v", err)
		}
	})
	if identity := session.InitializeResult().ServerInfo; identity.Name != "local-runtime-mcp" || identity.Version != mcpserver.Version {
		t.Fatalf("unexpected executable identity: %+v", identity)
	}
	catalog, err := session.ListTools(ctx, nil)
	if err != nil || len(catalog.Tools) != 21 {
		t.Fatalf("executable tool catalog: %v; %v", catalog, err)
	}
	call := func(name string, arguments any, output any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("%s: %v; result=%+v", name, err, result)
		}
		if output != nil {
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, output); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	var version runtimeprocess.Result
	call("process_run", map[string]any{"program": executable, "args": []string{"version"}}, &version)
	if version.Running || version.ExitCode != 0 || strings.TrimSpace(version.Stdout) != "lrmcp "+mcpserver.Version {
		t.Fatalf("version process: %+v", version)
	}
	// Exercise duration through the actual installed binary without requiring
	// a platform shell. Delay collection after the helper reports completion.
	if err := os.WriteFile(filepath.Join(command.Dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	requested := time.Now()
	var runningHelper runtimeprocess.Result
	call("process_run", map[string]any{
		"program": os.Args[0], "args": []string{"-test.run=^TestRuntimeProcessHelper$", "--", "release", command.Dir},
		"environment":   map[string]string{"LOCAL_RUNTIME_MCP_RUNTIME_PROCESS_HELPER": "1", "GORACE": "atexit_sleep_ms=0"},
		"yield_time_ms": 1, "timeout_seconds": 10,
	}, &runningHelper)
	if !runningHelper.Running || runningHelper.SessionID == "" {
		t.Fatalf("expected persistent helper process: %+v", runningHelper)
	}
	var helperTiming processHelperTiming
	waitForProcessHelperFile(t, ctx, filepath.Join(command.Dir, "release.done"), &helperTiming)
	time.Sleep(2 * time.Second)
	var collectedHelper runtimeprocess.Result
	call("process_continue", map[string]any{"session_id": runningHelper.SessionID, "yield_time_ms": 1000}, &collectedHelper)
	upperBound := time.Unix(0, helperTiming.FinishedNS).Sub(requested).Milliseconds() + 1000
	if collectedHelper.Running || collectedHelper.ExitCode != 0 || collectedHelper.DurationMS > upperBound {
		t.Fatalf("executable duration includes collection delay: result=%+v upper_bound=%dms", collectedHelper, upperBound)
	}
	output := strings.ReplaceAll(runningHelper.Stdout+collectedHelper.Stdout, "\r\n", "\n")
	if output != "release tick=1\nrelease tick=2\nrelease tick=3\n" || collectedHelper.Stderr != "" {
		t.Fatalf("executable helper output: stdout=%q stderr=%q", output, collectedHelper.Stderr)
	}
	path := filepath.Join(command.Dir, "release.txt")
	var written filesystem.TextWriteResult
	call("filesystem_write_text", map[string]any{"path": path, "mode": "create", "content": "release smoke\n"}, &written)
	call("filesystem_patch_text", map[string]any{"path": path, "expected_sha256": written.SHA256, "hunks": []map[string]string{{"old": "smoke", "new": "verified"}}}, nil)
	var read filesystem.TextReadResult
	call("filesystem_read_text", map[string]any{"path": path, "with_sha256": true}, &read)
	if read.Content != "release verified\n" || read.SHA256 == written.SHA256 {
		t.Fatalf("text contract: %+v", read)
	}
	call("filesystem_write_text", map[string]any{"path": path, "mode": "replace", "expected_sha256": read.SHA256, "content": "release final\n"}, nil)
	call("filesystem_stat", map[string]any{"path": path, "with_sha256": true}, nil)
	call("filesystem_list", map[string]any{"path": command.Dir}, nil)
	var search filesystem.SearchResult
	call("filesystem_search_text", map[string]any{"path": command.Dir, "query": "final"}, &search)
	if len(search.Matches) != 1 {
		t.Fatalf("text search: %+v", search)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(command.Dir, "release.png")
	if err := os.WriteFile(imagePath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	result := call("image_read", map[string]any{"path": imagePath}, nil)
	if len(result.Content) == 0 {
		t.Fatal("missing native image content")
	}
	content, ok := result.Content[0].(*mcp.ImageContent)
	if !ok || content.MIMEType != "image/png" || !bytes.Equal(content.Data, encoded.Bytes()) {
		t.Fatal("incorrect native image content")
	}
	call("browser_status", map[string]any{}, nil)
	var control computer.ControlStatus
	call("computer_control", map[string]any{"kind": "status"}, &control)
	if control.ControlID != "" || control.Status == "owned" {
		t.Fatalf("unexpected desktop owner: %+v", control)
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		var running runtimeprocess.Result
		call("process_run", map[string]any{"program": "pwsh", "args": []string{"-NoProfile", "-Command", "Start-Sleep -Seconds 20"}, "yield_time_ms": 1}, &running)
		if !running.Running || running.SessionID == "" {
			t.Fatalf("expected persistent process session: %+v", running)
		}
		var terminated runtimeprocess.Result
		call("process_continue", map[string]any{"session_id": running.SessionID, "terminate": true, "yield_time_ms": 1000}, &terminated)
		if terminated.Running {
			t.Fatal("terminated process is still running")
		}
	}
}
