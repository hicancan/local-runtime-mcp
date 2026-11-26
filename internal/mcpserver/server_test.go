package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
		InboundDir:   t.TempDir(),
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
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, target); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i := len(res.Content) - 1; i >= 0; i-- {
		if tc, ok := res.Content[i].(*mcp.TextContent); ok && strings.TrimSpace(tc.Text) != "" {
			if err := json.Unmarshal([]byte(tc.Text), target); err != nil {
				t.Fatalf("unmarshal text content %q: %v", tc.Text, err)
			}
			return
		}
	}
	t.Fatalf("no structured or JSON text content in CallToolResult: %+v", res.Content)
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
		if tl.Name == "run" || tl.Name == "wait" {
			if tl.OutputSchema != nil {
				t.Fatalf("tool %s must omit outputSchema so hosts pass native ImageContent unimpeded, got %#v", tl.Name, tl.OutputSchema)
			}
		}
		if tl.Name == "run" {
			// Verify OpenAI Apps SDK _meta["openai/fileParams"] contract on `run`.
			fp, ok := tl.Meta["openai/fileParams"]
			if !ok {
				t.Fatalf("run tool missing _meta[openai/fileParams]: %#v", tl.Meta)
			}
			rawFP, _ := json.Marshal(fp)
			if string(rawFP) != `["files"]` {
				t.Fatalf("expected _meta[openai/fileParams]=[\"files\"], got %s", rawFP)
			}
			rawSchema, _ := json.Marshal(tl.InputSchema)
			var schemaObj map[string]any
			if err := json.Unmarshal(rawSchema, &schemaObj); err != nil {
				t.Fatalf("unmarshal run inputSchema: %v", err)
			}
			props, _ := schemaObj["properties"].(map[string]any)
			filesSchema, _ := props["files"].(map[string]any)
			if filesSchema["type"] != "array" {
				t.Fatalf("expected files schema type=array, got %#v", filesSchema)
			}
			items, _ := filesSchema["items"].(map[string]any)
			itemProps, _ := items["properties"].(map[string]any)
			for _, k := range []string{"download_url", "file_id", "mime_type", "file_name"} {
				if _, exists := itemProps[k]; !exists {
					t.Fatalf("files item schema missing required OpenAI property %q: %#v", k, itemProps)
				}
			}
			rawReq, _ := json.Marshal(items["required"])
			if string(rawReq) != `["download_url","file_id"]` {
				t.Fatalf("expected files item required=[\"download_url\",\"file_id\"], got %s", rawReq)
			}
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
	if runRes.StructuredContent != nil {
		t.Fatalf("run must omit StructuredContent so ChatGPT passes ImageContent natively, got %#v", runRes.StructuredContent)
	}
	if len(runRes.Content) != 2 {
		t.Fatalf("expected [ImageContent, TextContent] when emitImage is used, got %d content blocks", len(runRes.Content))
	}
	ic, ok := runRes.Content[0].(*mcp.ImageContent)
	if !ok || ic.MIMEType != "image/png" || len(ic.Data) == 0 {
		t.Fatalf("expected Content[0] to be native MCP ImageContent, got %T", runRes.Content[0])
	}
	tc, ok := runRes.Content[1].(*mcp.TextContent)
	if !ok || tc.Text == "" {
		t.Fatalf("expected Content[1] to be JSON TextContent, got %T", runRes.Content[1])
	}
	var runCell jsrt.CellResult
	if err := json.Unmarshal([]byte(tc.Text), &runCell); err != nil || runCell.Status != "done" {
		t.Fatalf("expected valid CellResult JSON in TextContent alongside ImageContent, got %s (%v)", tc.Text, err)
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

	// Follow-up ad-hoc `run` MUST NOT resurrect the completed task session.
	callOK(t, sess, "run", map[string]any{
		"title": "后续独立查询",
		"code":  `1 + 1`,
	})
	sessionsAfter, err := store.ListSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	foundDone := false
	for _, s := range sessionsAfter {
		if s.ID == sOut.SessionID {
			foundDone = true
			if s.Status != "done" {
				t.Fatalf("completed session %s was resurrected to status=%q", s.ID, s.Status)
			}
		}
	}
	if !foundDone {
		t.Fatalf("completed session %s missing after follow-up run", sOut.SessionID)
	}

	// `run` accepts optional explicit session_id matching session(start).
	sRes2 := callOK(t, sess, "session", map[string]any{
		"action": "start",
		"title":  "显式会话绑定测试",
	})
	var sOut2 sessionOutput
	decodeStructured(t, sRes2, &sOut2)
	callOK(t, sess, "run", map[string]any{
		"title":      "带显式 session_id 执行",
		"session_id": sOut2.SessionID,
		"code":       `40 + 2`,
	})
	ev2, err := store.ListEvents(audit.EventFilter{SessionID: sOut2.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	matchedRun := false
	for _, e := range ev2 {
		if e.ToolName == "run" && e.Summary == "带显式 session_id 执行" {
			matchedRun = true
		}
	}
	if !matchedRun {
		t.Fatalf("explicit run session_id %s was not audited under target session: %+v", sOut2.SessionID, ev2)
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
	decodeStructured(t, r1, &c1)
	if c1.Status != "running" || c1.CellID == "" {
		t.Fatalf("expected running cell_id, got %+v", c1)
	}
	r2 := callOK(t, sess, "wait", map[string]any{"cell_id": c1.CellID, "yield_ms": 2000})
	if r2.StructuredContent != nil {
		t.Fatalf("wait must omit StructuredContent, got %#v", r2.StructuredContent)
	}
	var c2 jsrt.CellResult
	decodeStructured(t, r2, &c2)
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

func TestRunInboundFilesBidirectionalAndSecurity(t *testing.T) {
	store, err := audit.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Build a valid 6x6 PNG payload and a UTF-8 text document on a mock file host.
	im := image.NewRGBA(image.Rect(0, 0, 6, 6))
	im.Set(2, 2, color.RGBA{R: 12, G: 180, B: 90, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, im); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/img":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBuf.Bytes())
		case "/note":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("hello from chatgpt inbound channel"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sess, managed := testSession(t, store, false)
	managed.inbound.allowInsecure = true
	managed.inbound.client = managed.inbound.buildHTTPClient()

	res1 := callOK(t, sess, "run", map[string]any{
		"title": "验证双向文件通道与图片回显",
		"files": []map[string]any{
			{
				"file_id":      "file-img-01",
				"download_url": srv.URL + "/img?sig=super-secret-token",
				"mime_type":    "image/png",
				"file_name":    "../../weird:name?.png",
			},
			{
				"file_id":      "file-txt-02",
				"download_url": srv.URL + "/note?sig=another-secret",
				"file_name":    "memo.txt",
			},
		},
		"code": `
			const im = await img.read(inputFile.path);
			emitImage(im, "roundtrip");
			const txt = await fs.read(inputFiles[1].path);
			return {
				count: inputFiles.length,
				firstName: inputFile.name,
				w: im.width,
				h: im.height,
				text: txt.content,
			};
		`,
	})
	if len(res1.Content) != 2 {
		t.Fatalf("expected [ImageContent, TextContent] on bidirectional round-trip, got %d", len(res1.Content))
	}
	if _, ok := res1.Content[0].(*mcp.ImageContent); !ok {
		t.Fatalf("expected ImageContent[0], got %T", res1.Content[0])
	}
	var cell1 jsrt.CellResult
	decodeStructured(t, res1, &cell1)
	if cell1.Status != "done" || len(cell1.InboundFiles) != 2 {
		t.Fatalf("unexpected bidirectional cell result: %+v", cell1)
	}
	valMap, _ := cell1.Value.(map[string]any)
	if valMap["text"] != "hello from chatgpt inbound channel" || valMap["w"] != float64(6) {
		t.Fatalf("unexpected JS bidirectional value: %#v", valMap)
	}
	if strings.Contains(cell1.InboundFiles[0].Name, "..") || strings.Contains(cell1.InboundFiles[0].Name, ":") {
		t.Fatalf("filename was not sanitized: %q", cell1.InboundFiles[0].Name)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected exactly 2 upstream downloads, got %d", hits.Load())
	}

	// Second call referencing the same file_id must hit local dedup cache (0 new HTTP downloads).
	res2 := callOK(t, sess, "run", map[string]any{
		"title": "验证重复 file_id 零重下缓存",
		"files": []map[string]any{
			{
				"file_id":      "file-txt-02",
				"download_url": srv.URL + "/note?sig=rotated-sig",
				"file_name":    "memo.txt",
			},
		},
		"code": `inputFile.file_id + ":" + inputFiles.length`,
	})
	var cell2 jsrt.CellResult
	decodeStructured(t, res2, &cell2)
	if cell2.Value != "file-txt-02:1" || hits.Load() != 2 {
		t.Fatalf("expected dedup cache hit (hits=2), got value=%#v hits=%d", cell2.Value, hits.Load())
	}

	// Subsequent normal call without files must reset inputFiles=[] and inputFile=null.
	res3 := callOK(t, sess, "run", map[string]any{
		"title": "验证无附件时清空 inputFile",
		"code":  `({ isNull: inputFile === null, len: inputFiles.length })`,
	})
	var cell3 jsrt.CellResult
	decodeStructured(t, res3, &cell3)
	m3, _ := cell3.Value.(map[string]any)
	if m3["isNull"] != true || m3["len"] != float64(0) {
		t.Fatalf("expected inputFile=null & inputFiles=[], got %#v", m3)
	}

	// Audit log must redact signed query tokens from download_url.
	events, err := store.ListEvents(audit.EventFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if strings.Contains(ev.ArgsJSON, "super-secret-token") || strings.Contains(ev.ArgsJSON, "another-secret") {
			t.Fatalf("signed download_url query secret leaked into audit args: %s", ev.ArgsJSON)
		}
	}

	// Read-only grant must reject inbound file writes.
	roSess, roManaged := testSession(t, nil, true)
	roManaged.inbound.allowInsecure = true
	roManaged.inbound.client = roManaged.inbound.buildHTTPClient()
	roRes, err := roSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "run",
		Arguments: map[string]any{
			"title": "只读拦截入站文件",
			"files": []map[string]any{{"file_id": "file-ro", "download_url": srv.URL + "/note"}},
			"code":  `1`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !roRes.IsError {
		t.Fatalf("expected read-only grant to reject inbound files")
	}

	// Production SSRF guard must block non-HTTPS, localhost, RFC1918, link-local, CGNAT, and ::1.
	blockedURLs := []string{
		"http://files.oaiusercontent.com/a.png",
		"https://localhost/a.png",
		"https://127.0.0.1/a.png",
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.1/a.png",
		"https://192.168.1.1/a.png",
		"https://100.64.0.1/a.png",
		"https://[::1]/a.png",
		"https://user:pass@files.oaiusercontent.com/a.png",
	}
	for _, bad := range blockedURLs {
		if _, err := validateInboundURL(bad, false); err == nil {
			t.Fatalf("expected SSRF guard to block %q", bad)
		}
	}
	if _, err := validateInboundURL("https://files.oaiusercontent.com/file-abc?sig=1", false); err != nil {
		t.Fatalf("expected valid OpenAI HTTPS download_url allowed, got %v", err)
	}
	// Transparent TUN proxy fake-IP (198.18.0.0/15) must remain permitted.
	if err := checkInboundIP(net.ParseIP("198.18.0.42")); err != nil {
		t.Fatalf("expected TUN fake-IP 198.18.0.42 allowed, got %v", err)
	}
}
