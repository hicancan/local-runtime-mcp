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

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/jsrt"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Release smoke test against the real compiled executable.
func TestExecutableStdio(t *testing.T) {
	srcExe := os.Getenv("LOCAL_RUNTIME_MCP_EXECUTABLE")
	if srcExe == "" {
		t.Skip("set LOCAL_RUNTIME_MCP_EXECUTABLE to a release executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

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
	cfgYAML := "browser:\n  listen: " + freePort() + "\n  token: release-smoke-test-credential-0001\n  disable_auto_cdp: true\n" +
		"local:\n  listen: " + freePort() + "\n  token: " + strings.Repeat("s", 32) + "\n"
	if err := os.WriteFile(filepath.Join(workDir, config.FileName), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	var cleanEnv []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), config.EnvironmentPrefix) {
			cleanEnv = append(cleanEnv, entry)
		}
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		cmd := exec.CommandContext(stopCtx, executable, "stop")
		cmd.Dir = workDir
		cmd.Env = cleanEnv
		_ = cmd.Run()
	})

	command := exec.Command(executable, "serve", "--transport", "stdio")
	command.Dir = workDir
	command.Env = cleanEnv
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
	if err != nil || len(catalog.Tools) != 3 {
		t.Fatalf("expected 3 core primitives (run, wait, session), got %+v err=%v", catalog, err)
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

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(command.Dir, "release.png")
	if err := os.WriteFile(imagePath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	textPath := filepath.Join(command.Dir, "release.txt")

	// Single-turn parallel verification of proc + fs + img + browser.status + desktop.control!
	var cell jsrt.CellResult
	res := call("run", map[string]any{
		"title":      "并发验收进程、文件、图片与状态",
		"done_title": "已完成全栈核心冒烟验收",
		"code": `
			const exe = ` + "`" + filepath.ToSlash(executable) + "`" + `;
			const txt = ` + "`" + filepath.ToSlash(textPath) + "`" + `;
			const imgFile = ` + "`" + filepath.ToSlash(imagePath) + "`" + `;
			const [ver, w, br, ctl, shot] = await Promise.all([
				proc.run(exe, ["version"]),
				fs.write(txt, "release smoke\n", { mode: "create" }),
				browser.status(),
				desktop.control("status"),
				img.read(imgFile),
			]);
			await fs.patch(txt, [{ old: "smoke", new: "verified" }], w.sha256);
			const rd = await fs.read(txt);
			emitImage(shot, "smoke-png");
			return {
				version: ver.stdout.trim(),
				content: rd.content.trim(),
				browserConfigured: br.configured,
				desktopStatus: ctl.status,
			};
		`,
	}, &cell)

	m, ok := cell.Value.(map[string]any)
	if !ok || m["version"] != "lrmcp "+mcpserver.Version || m["content"] != "release verified" || m["desktopStatus"] != "free" {
		t.Fatalf("unexpected smoke value: %+v", cell.Value)
	}
	hasPNG := false
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok && ic.MIMEType == "image/png" && bytes.Equal(ic.Data, encoded.Bytes()) {
			hasPNG = true
		}
	}
	if !hasPNG {
		t.Fatal("missing native MCP ImageContent from emitImage")
	}

	// Verify async yield + wait + terminate
	var asyncCell jsrt.CellResult
	call("run", map[string]any{
		"title":    "测试异步挂起与取消",
		"yield_ms": 40,
		"code":     `await sleep(5000); return "never";`,
	}, &asyncCell)
	if asyncCell.Status != "running" || asyncCell.CellID == "" {
		t.Fatalf("expected running cell_id, got %+v", asyncCell)
	}
	var canceled jsrt.CellResult
	call("wait", map[string]any{"cell_id": asyncCell.CellID, "terminate": true}, &canceled)
	if canceled.Status != "canceled" {
		t.Fatalf("expected canceled cell, got %+v", canceled)
	}
}

// TestExecutableDetachedDaemonLifecycleAndHotPlug verifies the unified daemon
// end-to-end against the real compiled binary.
func TestExecutableDetachedDaemonLifecycleAndHotPlug(t *testing.T) {
	srcExe := os.Getenv("LOCAL_RUNTIME_MCP_EXECUTABLE")
	if srcExe == "" {
		t.Skip("set LOCAL_RUNTIME_MCP_EXECUTABLE to a release executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

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
	cfgYAML := "browser:\n  listen: " + browserAddr + "\n  token: " + strings.Repeat("b", 32) + "\n  disable_auto_cdp: true\n" +
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

	first := connectStdio("first-ide")
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

	if res, err := second.CallTool(ctx, &mcp.CallToolParams{Name: "session", Arguments: map[string]any{
		"action":  "start",
		"title":   "桌面巡检任务",
		"message": "已完成连接稳定性检查，准备验证审计流。",
	}}); err != nil || res.IsError {
		t.Fatalf("session start failed: %v %+v", err, res)
	}
	if res, err := second.CallTool(ctx, &mcp.CallToolParams{Name: "run", Arguments: map[string]any{
		"title":      "核对浏览器引擎状态",
		"done_title": "已核对浏览器引擎状态",
		"code":       "return await browser.status();",
	}}); err != nil || res.IsError {
		t.Fatalf("second stdio run failed: %v %+v", err, res)
	}

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
		t.Fatalf("audit list missing attributed session: %+v", auditSessions)
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
	if !strings.Contains(exported, "已核对浏览器引擎状态") || !strings.Contains(exported, "稳定性检查") {
		t.Fatalf("audit export missing expected JSONL records: %s", exported)
	}

	var uiBoot struct {
		BootstrapURL string `json:"bootstrap_url"`
	}
	if err := json.Unmarshal(runCLI("ui", "--no-open"), &uiBoot); err != nil || !strings.Contains(uiBoot.BootstrapURL, "/ui/auth?code=") {
		t.Fatalf("ui --no-open failed: %v %+v", err, uiBoot)
	}
	noRedirect := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	uiResp, err := noRedirect.Get(uiBoot.BootstrapURL)
	if err != nil {
		t.Fatal(err)
	}
	uiResp.Body.Close()
	if uiResp.StatusCode != http.StatusSeeOther || uiResp.Header.Get("Location") != "/ui/" {
		t.Fatalf("ui bootstrap auth failed: status=%d loc=%q", uiResp.StatusCode, uiResp.Header.Get("Location"))
	}

	// Hot-plug HTTP wire + apply read-only grant + verify proc.run rejected over HTTP!
	runCLI("tunnel", "up", "http")
	runCLI("grant", "remote", "http", "--read-only")

	httpClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	callBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run","arguments":{"title":"blocked write","code":"await proc.run('echo',['no'])"}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+httpAddr+"/mcp", strings.NewReader(callBody))
	req.Header.Set("Authorization", "Bearer "+httpToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("remote HTTP request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "read-only") {
		t.Fatalf("remote read-only enforcement failed: HTTP %d %s", resp.StatusCode, string(body))
	}

	runCLI("tunnel", "down", "http")
	_ = second.Close()
	runCLI("stop")
}
