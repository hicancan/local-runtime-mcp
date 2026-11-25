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

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	"github.com/hicancan/local-runtime-mcp/internal/jsrt"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testSession(t *testing.T, store *audit.Store, readOnly bool) (*mcp.ClientSession, *Managed) {
	t.Helper()
	ctx := context.Background()
	procMgr := runtimeprocess.NewManager(ctx)
	t.Cleanup(func() { _ = procMgr.Close() })
	br, err := browser.Start(ctx, config.Browser{DisableAutoCDP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close(context.Background()) })
	ctrl := computer.New(ctx)
	t.Cleanup(func() { _ = ctrl.Close() })
	capDir := t.TempDir()
	reg, err := capability.Load(capDir, br)
	if err != nil {
		t.Fatal(err)
	}
	fsSvc := filesystem.New()
	eng := jsrt.NewEngine(ctx, jsrt.Dependencies{
		Process:      procMgr,
		Filesystem:   fsSvc,
		Browser:      br,
		Computer:     ctrl,
		Capabilities: reg,
		LocalApps:    true,
	})
	t.Cleanup(eng.Close)

	managed := NewManaged(Dependencies{
		Runtime:      eng,
		Process:      procMgr,
		Filesystem:   fsSvc,
		Browser:      br,
		Computer:     ctrl,
		Capabilities: reg,
		Audit:        store,
		Wire:         "local",
		LocalApps:    true,
		ReadOnly:     readOnly,
	})

	cSide, sSide := mcp.NewInMemoryTransports()
	if _, err := managed.Server.Connect(ctx, sSide, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	sess, err := client.Connect(ctx, cSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, managed
}

func decodeStructured(t *testing.T, res *mcp.CallToolResult, target any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		t.Fatal(err)
	}
}

func callOK(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) transport error: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool(%s) returned error: %+v", name, res.Content)
	}
	return res
}

func TestMinimalFourPrimitiveCatalog(t *testing.T) {
	sess, _ := testSession(t, nil, false)
	listed, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"run":                true,
		"wait":               true,
		"session":            true,
		"capability_propose": true,
	}
	if len(listed.Tools) != len(want) {
		names := make([]string, len(listed.Tools))
		for i, tl := range listed.Tools {
			names[i] = tl.Name
		}
		t.Fatalf("expected exactly 4 orthogonal primitives %v, got %d: %v", want, len(listed.Tools), names)
	}
	for _, tl := range listed.Tools {
		if !want[tl.Name] {
			t.Fatalf("unexpected tool in v18 catalog: %s", tl.Name)
		}
	}
}

func TestRunDualStateSummaryImagesAndAudit(t *testing.T) {
	store, err := audit.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, _ := testSession(t, store, false)

	// Open named task session
	sRes := callOK(t, sess, "session", map[string]any{
		"action":  "start",
		"title":   "多工具并行巡检",
		"message": "启动任务线",
	})
	var sOut sessionOutput
	b, _ := json.Marshal(sRes.StructuredContent)
	_ = json.Unmarshal(b, &sOut)
	if !sOut.Recorded || sOut.SessionID == "" {
		t.Fatalf("unexpected session start output: %+v", sOut)
	}

	// Prepare test file & PNG image
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "dot.png")
	im := image.NewRGBA(image.Rect(0, 0, 4, 4))
	im.Set(1, 1, color.RGBA{G: 200, A: 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, im)
	_ = os.WriteFile(imgPath, buf.Bytes(), 0o600)

	// Call `run` with dual-state title -> done_title and native ImageContent emission
	runRes := callOK(t, sess, "run", map[string]any{
		"title":      "正在读写测试文件并输出图片",
		"done_title": "已完成测试文件校验与图片输出",
		"code": `
			const f = ` + "`" + filepath.ToSlash(filepath.Join(dir, "a.txt")) + "`" + `;
			const p = ` + "`" + filepath.ToSlash(imgPath) + "`" + `;
			await fs.write(f, "hello v18\n", { mode: "create" });
			const [rd, shot] = await Promise.all([fs.read(f), img.read(p)]);
			emitImage(shot, "dot");
			return { text: rd.content.trim(), w: shot.width };
		`,
	})
	hasImage := false
	for _, c := range runRes.Content {
		if ic, ok := c.(*mcp.ImageContent); ok && ic.MIMEType == "image/png" && len(ic.Data) > 0 {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatalf("expected native MCP ImageContent from emitImage inside run")
	}

	// Verify audit recorded done_title and image blob under the active task session
	events, err := store.ListEvents(audit.EventFilter{SessionID: sOut.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	foundRun := false
	for _, ev := range events {
		if ev.ToolName == "run" {
			foundRun = true
			if ev.Summary != "已完成测试文件校验与图片输出" {
				t.Fatalf("expected done_title flip in audit summary, got %q", ev.Summary)
			}
			if ev.ImageBlob == "" {
				t.Fatalf("expected emitted image persisted to audit blob store")
			}
		}
	}
	if !foundRun {
		t.Fatalf("missing audited run event under session %s: %+v", sOut.SessionID, events)
	}

	// Finish session cleanly
	callOK(t, sess, "session", map[string]any{
		"action":  "done",
		"message": "任务完成",
	})
	sessions, err := store.ListSessions(10)
	if err != nil || len(sessions) == 0 || sessions[0].Status != "done" {
		t.Fatalf("expected session status=done, got %+v err=%v", sessions, err)
	}
}

func TestRunWaitAndReadOnlyGuard(t *testing.T) {
	sess, _ := testSession(t, nil, false)
	r1 := callOK(t, sess, "run", map[string]any{
		"title":    "后台等待测试",
		"yield_ms": 40,
		"code":     `await sleep(160); return "ok-async";`,
	})
	var c1 jsrt.CellResult
	b1, _ := json.Marshal(r1.StructuredContent)
	_ = json.Unmarshal(b1, &c1)
	if c1.Status != "running" || c1.CellID == "" {
		t.Fatalf("expected running cell_id, got %+v", c1)
	}
	r2 := callOK(t, sess, "wait", map[string]any{"cell_id": c1.CellID, "yield_ms": 2000})
	var c2 jsrt.CellResult
	b2, _ := json.Marshal(r2.StructuredContent)
	_ = json.Unmarshal(b2, &c2)
	if c2.Status != "done" || c2.Value != "ok-async" {
		t.Fatalf("expected wait completion, got %+v", c2)
	}

	// Read-only server must reject mutations inside `run`
	roSess, _ := testSession(t, nil, true)
	roRes, err := roSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "run",
		Arguments: map[string]any{
			"title": "尝试越权写文件",
			"code":  `await fs.write("` + filepath.ToSlash(filepath.Join(t.TempDir(), "x.txt")) + `", "nope", { mode: "create" });`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !roRes.IsError {
		t.Fatalf("read-only run should have rejected fs.write")
	}
	raw, _ := json.Marshal(roRes.Content)
	if !strings.Contains(string(raw), "read-only") {
		t.Fatalf("expected read-only error message, got %s", raw)
	}
}
