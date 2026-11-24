package runtimehost

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	command := exec.Command(executable, "serve", "--transport", "stdio", "--browser-listen", "127.0.0.1:0", "--browser-token", "release-smoke-test-credential-0001")
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
	if err != nil || len(catalog.Tools) != 23 {
		t.Fatalf("executable tool catalog: %v; %v", catalog, err)
	}
	call := func(name string, arguments any, output any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
		if err != nil || result == nil || result.IsError {
			var message string
			if result != nil {
				for _, content := range result.Content {
					if text, ok := content.(*mcp.TextContent); ok {
						message += text.Text
					}
				}
			}
			t.Fatalf("%s: %v; tool error: %s", name, err, message)
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
	call("process_continue", map[string]any{"session_id": runningHelper.SessionID, "output_cursor": runningHelper.OutputCursor, "yield_time_ms": 1000}, &collectedHelper)
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
	if runtime.GOOS == "windows" && os.Getenv("LOCAL_RUNTIME_MCP_DESKTOP_SMOKE") == "1" {
		var state computer.State
		result := call("computer_state", map[string]any{}, &state)
		if len(result.Content) != 1 {
			t.Fatal("desktop state did not return exactly one image block")
		}
		content, ok := result.Content[0].(*mcp.ImageContent)
		if !ok || content.MIMEType != "image/png" || len(content.Data) == 0 {
			t.Fatal("desktop state is missing native PNG pixels")
		}
		pixels, err := png.DecodeConfig(bytes.NewReader(content.Data))
		if err != nil || pixels.Width != state.Width || pixels.Height != state.Height || state.Actionable || state.StateID != "" {
			t.Fatal("desktop PNG and read-only structured state are inconsistent")
		}
		t.Logf("raw computer_state: native PNG %d bytes, %dx%d, structured UIA=%s", len(content.Data), pixels.Width, pixels.Height, state.UIAStatus)
	}
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

// TestExecutableDetachedDaemonLifecycleAndHotPlug proves the core v16.0.0
// unified daemon contract end-to-end against the real compiled binary:
//  1. First `serve --transport stdio` auto-spawns a detached user-session daemon.
//  2. Second `serve --transport stdio` attaches to that same daemon concurrently.
//  3. Closing the FIRST stdio client NEVER stops the daemon or breaks the second client.
//  4. `tunnel up http` / `grant remote http --read-only` / `tunnel down http`
//     operate live with ZERO daemon restarts.
//  5. `lrmcp stop` gracefully terminates the detached daemon.
func TestExecutableDetachedDaemonLifecycleAndHotPlug(t *testing.T) {
	srcExe := os.Getenv("LOCAL_RUNTIME_MCP_EXECUTABLE")
	if srcExe == "" {
		t.Skip("set LOCAL_RUNTIME_MCP_EXECUTABLE to a release executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Copy the executable into an isolated TempDir so its adjacent
	// local-runtime-mcp.yaml does not touch any real user configuration.
	workDir := t.TempDir()
	binName := "lrmcp"
	if runtime.GOOS == "windows" {
		binName = "lrmcp.exe"
	}
	executable := filepath.Join(workDir, binName)
	srcData, err := os.ReadFile(srcExe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, srcData, 0o755); err != nil {
		t.Fatal(err)
	}

	freePort := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}
	localAddr := freePort()
	browserAddr := freePort()
	httpAddr := freePort()
	httpToken := strings.Repeat("h", 32)
	cfgYAML := "browser:\n  listen: " + browserAddr + "\n  token: " + strings.Repeat("b", 32) + "\n" +
		"local:\n  listen: " + localAddr + "\n  token: " + strings.Repeat("l", 32) + "\n" +
		"http:\n  listen: " + httpAddr + "\n  auth: bearer\n  bearer_token: " + httpToken + "\n"
	if err := os.WriteFile(filepath.Join(workDir, config.FileName), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanEnv := func() []string {
		var env []string
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(entry), config.EnvironmentPrefix) {
				env = append(env, entry)
			}
		}
		return env
	}
	runCLI := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, executable, args...)
		cmd.Dir = workDir
		cmd.Env = cleanEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("lrmcp %v failed: %v\n%s", args, err, string(out))
		}
		return out
	}

	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		cmd := exec.CommandContext(stopCtx, executable, "stop")
		cmd.Dir = workDir
		cmd.Env = cleanEnv()
		_ = cmd.Run()
	})

	connectStdio := func(name string) *mcp.ClientSession {
		t.Helper()
		cmd := exec.Command(executable, "serve", "--transport", "stdio", "--agent", name)
		cmd.Dir = workDir
		cmd.Env = cleanEnv()
		var diag bytes.Buffer
		cmd.Stderr = &diag
		session, err := mcp.NewClient(&mcp.Implementation{Name: "shared-ide-name", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{
			Command: cmd, TerminateDuration: 5 * time.Second,
		}, nil)
		if err != nil {
			t.Fatalf("connect %s: %v (stderr=%s)", name, err, diag.String())
		}
		return session
	}

	// 1. First stdio client auto-spawns the detached daemon.
	first := connectStdio("first-ide")
	// 2. Second stdio client attaches concurrently to the same daemon.
	second := connectStdio("second-ide")
	defer func() { _ = second.Close() }()

	var st1 struct {
		Running bool `json:"running"`
		Daemon  struct {
			PID           int `json:"pid"`
			AttachedStdio int `json:"attached_stdio"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(runCLI("status"), &st1); err != nil {
		t.Fatal(err)
	}
	if !st1.Running || st1.Daemon.PID <= 0 || st1.Daemon.AttachedStdio != 2 {
		t.Fatalf("unexpected initial daemon status: %+v", st1)
	}

	// 3. Close the FIRST stdio client — detached daemon & second client MUST survive!
	if err := first.Close(); err != nil {
		t.Fatalf("close first stdio: %v", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		var st2 struct {
			Running bool `json:"running"`
			Daemon  struct {
				PID           int `json:"pid"`
				AttachedStdio int `json:"attached_stdio"`
			} `json:"daemon"`
		}
		if err := json.Unmarshal(runCLI("status"), &st2); err != nil {
			t.Fatal(err)
		}
		if st2.Running && st2.Daemon.PID == st1.Daemon.PID && st2.Daemon.AttachedStdio == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not survive first stdio close cleanly: %+v", st2)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if res, err := second.CallTool(ctx, &mcp.CallToolParams{Name: "browser_status", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("second stdio client broke after first closed: %v %+v", err, res)
	}
	if res, err := second.CallTool(ctx, &mcp.CallToolParams{Name: "agent_report", Arguments: map[string]any{
		"title":   "桌面巡检任务",
		"message": "已完成连接稳定性检查，准备验证审计流。",
		"status":  "working",
	}}); err != nil || res.IsError {
		t.Fatalf("agent_report failed: %v %+v", err, res)
	}

	// Verify embedded SQLite WAL + FTS5 audit CLI commands & single-use Studio UI bootstrap!
	var auditSessions struct {
		Sessions []struct {
			ID      string `json:"id"`
			AgentID string `json:"agent_id"`
			Title   string `json:"title"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(runCLI("audit", "list"), &auditSessions); err != nil {
		t.Fatal(err)
	}
	foundTitle := false
	for _, s := range auditSessions.Sessions {
		if s.Title == "桌面巡检任务" && s.AgentID == "second-ide" {
			foundTitle = true
		}
	}
	if !foundTitle {
		t.Fatalf("audit list missing --agent attributed session (want agent_id=second-ide, title=桌面巡检任务): %+v", auditSessions)
	}
	var auditSearch struct {
		Events []struct {
			Kind    string `json:"kind"`
			Summary string `json:"summary"`
		} `json:"events"`
	}
	if err := json.Unmarshal(runCLI("audit", "search", "稳定性检查"), &auditSearch); err != nil {
		t.Fatal(err)
	}
	if len(auditSearch.Events) == 0 || auditSearch.Events[0].Kind != "note" {
		t.Fatalf("audit FTS5 search failed: %+v", auditSearch)
	}
	exported := string(runCLI("audit", "export"))
	if !strings.Contains(exported, "browser_status") || !strings.Contains(exported, "稳定性检查") {
		t.Fatalf("audit export missing expected JSONL records: %s", exported)
	}
	var uiBoot struct {
		BootstrapURL string `json:"bootstrap_url"`
	}
	if err := json.Unmarshal(runCLI("ui", "--no-open"), &uiBoot); err != nil || !strings.Contains(uiBoot.BootstrapURL, "/ui/auth?code=") {
		t.Fatalf("ui --no-open failed: %v %+v", err, uiBoot)
	}
	noRedirect := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	uiResp, err := noRedirect.Get(uiBoot.BootstrapURL)
	if err != nil {
		t.Fatal(err)
	}
	uiResp.Body.Close()
	if uiResp.StatusCode != http.StatusSeeOther || uiResp.Header.Get("Location") != "/ui/" {
		t.Fatalf("ui bootstrap auth failed: status=%d loc=%q", uiResp.StatusCode, uiResp.Header.Get("Location"))
	}
	replayResp, err := noRedirect.Get(uiBoot.BootstrapURL)
	if err != nil {
		t.Fatal(err)
	}
	replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed ui bootstrap URL was not rejected: %d", replayResp.StatusCode)
	}

	// 4. Hot-plug remote HTTP wire live (0 daemon restart), apply read-only remote grant, then unplug.
	runCLI("tunnel", "up", "http")
	runCLI("grant", "remote", "http", "--read-only")

	// Verify daemon PID never changed.
	var st3 struct {
		Daemon struct {
			PID int `json:"pid"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(runCLI("status"), &st3); err != nil {
		t.Fatal(err)
	}
	if st3.Daemon.PID != st1.Daemon.PID {
		t.Fatalf("daemon restarted during tunnel hot-plug: before=%d after=%d", st1.Daemon.PID, st3.Daemon.PID)
	}

	// Verify public HTTP responds to MCP initialize/tools/list with read-only tools only.
	httpClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+httpAddr+"/mcp", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+httpToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("remote HTTP request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), `"process_run"`) || !strings.Contains(string(body), `"filesystem_read_text"`) {
		t.Fatalf("remote read-only filter failed: HTTP %d %s", resp.StatusCode, string(body))
	}

	runCLI("tunnel", "down", "http")

	// 5. Stop daemon gracefully.
	_ = second.Close()
	runCLI("stop")
	var stEnd struct {
		Running bool `json:"running"`
	}
	if err := json.Unmarshal(runCLI("status"), &stEnd); err != nil {
		t.Fatal(err)
	}
	if stEnd.Running {
		t.Fatal("daemon still running after lrmcp stop")
	}
}
