package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"golang.org/x/net/websocket"
)

func startMockCDPServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var mu sync.Mutex
	targets := map[string]map[string]any{
		"T1": {"targetId": "T1", "type": "page", "title": "Home", "url": "https://example.com"},
	}
	targetSeq := 0
	docRequested := map[string]bool{}
	sessTarget := map[string]string{}
	axCount := 0
	var traceStreamData string

	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		var writeMu sync.Mutex
		sendJSON := func(v any) {
			b, _ := json.Marshal(v)
			writeMu.Lock()
			_ = websocket.Message.Send(ws, string(b))
			writeMu.Unlock()
		}
		for {
			var raw string
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			var req cdpRequest
			if json.Unmarshal([]byte(raw), &req) != nil {
				continue
			}
			var params map[string]any
			if b, err := json.Marshal(req.Params); err == nil {
				_ = json.Unmarshal(b, &params)
			}
			switch req.Method {
			case "Browser.getVersion":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"product": "Edg/158.0.0.0", "userAgent": "MockEdge/158"}})
			case "Target.getTargets":
				mu.Lock()
				infos := make([]map[string]any, 0, len(targets))
				for _, info := range targets {
					infos = append(infos, info)
				}
				mu.Unlock()
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"targetInfos": infos}})
			case "Target.createTarget":
				u, _ := params["url"].(string)
				mu.Lock()
				targetSeq++
				id := "T_NEW_" + strings.ReplaceAll(strings.ReplaceAll(u, "https://", ""), ":", "_") + "_" + string(rune('0'+targetSeq))
				targets[id] = map[string]any{"targetId": id, "type": "page", "title": "", "url": u}
				mu.Unlock()
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"targetId": id}})
			case "Target.closeTarget":
				tid, _ := params["targetId"].(string)
				mu.Lock()
				delete(targets, tid)
				mu.Unlock()
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"success": true}})
			case "Target.attachToTarget":
				tid, _ := params["targetId"].(string)
				sessID := "S_" + tid
				mu.Lock()
				sessTarget[sessID] = tid
				mu.Unlock()
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"sessionId": sessID}})
			case "Page.navigate":
				u, _ := params["url"].(string)
				mu.Lock()
				tid := sessTarget[req.SessionID]
				if info := targets[tid]; info != nil {
					info["url"] = u
					info["title"] = "Opened " + u
				}
				loaderID := "L_" + tid
				mu.Unlock()
				// Real Chromium order: Network.requestWillBeSent (Document) fires BEFORE Page.frameNavigated!
				sendJSON(map[string]any{
					"method":    "Network.requestWillBeSent",
					"sessionId": req.SessionID,
					"params": map[string]any{
						"requestId": "doc-" + tid,
						"loaderId":  loaderID,
						"type":      "Document",
						"request":   map[string]any{"url": u, "method": "GET"},
					},
				})
				sendJSON(map[string]any{
					"method":    "Network.responseReceived",
					"sessionId": req.SessionID,
					"params": map[string]any{
						"requestId": "doc-" + tid,
						"type":      "Document",
						"response":  map[string]any{"status": 200, "statusText": "OK", "mimeType": "text/html"},
					},
				})
				sendJSON(map[string]any{
					"method":    "Page.frameNavigated",
					"sessionId": req.SessionID,
					"params": map[string]any{
						"frame": map[string]any{"id": "F_" + tid, "loaderId": loaderID, "url": u},
					},
				})
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"frameId": "F_" + tid, "loaderId": loaderID}})
				sendJSON(map[string]any{"method": "Page.loadEventFired", "sessionId": req.SessionID, "params": map[string]any{"timestamp": 1.0}})
			case "DOM.getDocument":
				mu.Lock()
				docRequested[req.SessionID] = true
				mu.Unlock()
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"root": map[string]any{"nodeId": 1}}})
			case "DOM.pushNodesByBackendIdsToFrontend":
				mu.Lock()
				ok := docRequested[req.SessionID]
				mu.Unlock()
				if !ok {
					sendJSON(map[string]any{"id": req.ID, "error": map[string]any{"code": -32000, "message": "Document needs to be requested first"}})
					continue
				}
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"nodeIds": []int{101}}})
			case "CSS.getComputedStyleForNode":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"computedStyle": []map[string]any{
					{"name": "display", "value": "inline-block"},
					{"name": "font-size", "value": "16px"},
				}}})
			case "CSS.getMatchedStylesForNode":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{
					"matchedCSSRules": []map[string]any{
						{
							"rule": map[string]any{
								"origin":       "user-agent",
								"selectorList": map[string]any{"text": "a:-webkit-any-link"},
								"style":        map[string]any{"cssProperties": []map[string]any{{"name": "color", "value": "-webkit-link"}}},
							},
						},
					},
				}})
			case "Network.enable":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
				sendJSON(map[string]any{
					"method":    "Network.requestWillBeSent",
					"sessionId": req.SessionID,
					"params": map[string]any{
						"requestId": "req-1",
						"type":      "Fetch",
						"request":   map[string]any{"url": "https://example.com/api/data", "method": "GET"},
					},
				})
				sendJSON(map[string]any{
					"method":    "Network.responseReceived",
					"sessionId": req.SessionID,
					"params": map[string]any{
						"requestId": "req-1",
						"type":      "Fetch",
						"response":  map[string]any{"status": 200, "statusText": "OK", "mimeType": "application/json"},
					},
				})
			case "Runtime.enable":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
				sendJSON(map[string]any{
					"method":    "Runtime.consoleAPICalled",
					"sessionId": req.SessionID,
					"params": map[string]any{
						"type": "warn",
						"args": []map[string]any{{"type": "string", "value": "deprecation notice"}},
					},
				})
			case "Network.getResponseBody":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"body": `{"ok":true}`, "base64Encoded": false}})
			case "Accessibility.getFullAXTree":
				mu.Lock()
				axCount++
				n := axCount
				mu.Unlock()
				nodes := []map[string]any{
					{"role": map[string]any{"value": "heading"}, "name": map[string]any{"value": "Welcome"}},
					{"role": map[string]any{"value": "button"}, "name": map[string]any{"value": "Search"}, "backendDOMNodeId": 42},
				}
				if n >= 2 {
					nodes = append(nodes, map[string]any{"role": map[string]any{"value": "StaticText"}, "name": map[string]any{"value": "3 results found"}})
				}
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"nodes": nodes}})
			case "DOM.getBoxModel":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"model": map[string]any{"content": []float64{10, 10, 110, 10, 110, 50, 10, 50}}}})
			case "Runtime.evaluate":
				expr, _ := params["expression"].(string)
				switch {
				case expr == "document.readyState":
					sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "complete"}}})
				case strings.Contains(expr, "performance.getEntriesByType('navigation')"):
					sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": map[string]any{
						"ttfb_ms": 45.2,
						"fcp_ms":  210.5,
						"lcp_ms":  320.4,
						"dcl_ms":  587.6,
						"load_ms": 588.2,
						"cls":     0.01,
					}}}})
				default:
					sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "eval:" + expr}}})
				}
			case "Page.getLayoutMetrics":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"visualViewport": map[string]any{"clientWidth": 800, "clientHeight": 600}}})
			case "Page.captureScreenshot":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("fakepng"))}})
			case "Tracing.start":
				traceStreamData = `{"traceEvents":[{"name":"navigationStart","cat":"blink.user_timing","ph":"R","ts":1000000},{"name":"firstContentfulPaint","cat":"loading","ph":"R","ts":1250000},{"name":"largestContentfulPaint::Candidate","cat":"loading","ph":"R","ts":1450000},{"name":"RunTask","cat":"toplevel","ph":"X","ts":1500000,"dur":85000}]}`
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
			case "Tracing.end":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
				sendJSON(map[string]any{"method": "Tracing.tracingComplete", "params": map[string]any{"stream": "stream-1"}})
			case "IO.read":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"data": traceStreamData, "eof": true}})
			case "HeapProfiler.takeHeapSnapshot":
				sendJSON(map[string]any{"method": "HeapProfiler.addHeapSnapshotChunk", "sessionId": req.SessionID, "params": map[string]any{"chunk": `{"snapshot":{"node_count":1}}`}})
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
			case "Debugger.setBreakpointByUrl":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"breakpointId": "bp-1"}})
			default:
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return srv, wsURL
}

func TestBrowserLevelCDPEngineFullSuite(t *testing.T) {
	srv, wsURL := startMockCDPServer(t)
	defer srv.Close()

	ctx := context.Background()
	bridge, err := Start(ctx, config.Browser{
		CDPEndpoints:   []string{wsURL},
		DisableAutoCDP: true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer bridge.Close(ctx)

	inst, err := bridge.Connect(ctx, "cdp-1")
	if err != nil || !inst.Connected || inst.Browser != "MockEdge/158" {
		t.Fatalf("Connect failed: %#v err=%v", inst, err)
	}

	// Tabs listing
	tabs, err := bridge.Tabs(ctx, "cdp-1")
	if err != nil || len(tabs) != 1 || tabs[0].URL != "https://example.com" {
		t.Fatalf("unexpected tabs: %#v err=%v", tabs, err)
	}
	tabID := tabs[0].ID

	// Snapshot 1 (full) + Snapshot 2 (incremental diff)
	s1, err := bridge.SnapshotWithOptions(ctx, tabID, SnapshotOptions{Diff: true})
	if err != nil || s1.Diff || !strings.Contains(s1.Text, "Welcome") {
		t.Fatalf("unexpected s1: %#v err=%v", s1, err)
	}
	s2, err := bridge.SnapshotWithOptions(ctx, tabID, SnapshotOptions{Diff: true})
	if err != nil || !s2.Diff || s2.AddedLines != 1 || !strings.Contains(s2.Text, "+ 3 results found") {
		t.Fatalf("expected incremental AX diff (+1 line), got %#v err=%v", s2, err)
	}

	// Passive Network + NetworkBody + Console
	time.Sleep(30 * time.Millisecond)
	reqs, err := bridge.Network(ctx, tabID, NetworkFilter{})
	if err != nil || len(reqs) != 1 || reqs[0].Status != 200 {
		t.Fatalf("unexpected network entries: %#v err=%v", reqs, err)
	}
	body, err := bridge.NetworkBody(ctx, tabID, "req-1")
	if err != nil || body.Body != `{"ok":true}` {
		t.Fatalf("unexpected network body: %#v err=%v", body, err)
	}
	logs, err := bridge.Console(ctx, tabID, ConsoleFilter{})
	if err != nil || len(logs) != 1 || logs[0].Level != "warn" {
		t.Fatalf("unexpected console logs: %#v err=%v", logs, err)
	}

	// Semantic Locator click (zero snapshot ping-pong)
	actRes, err := bridge.Act(ctx, Action{
		TabID:   tabID,
		Kind:    "click",
		Locator: Locator{Role: "button", Name: "Search", Exact: true},
	})
	if err != nil || !actRes.Success {
		t.Fatalf("semantic locator click failed: %#v err=%v", actRes, err)
	}

	// Browser-process-level Tracing.start / Tracing.stop + Web Vitals parsing
	tmpDir := t.TempDir()
	tracePath := filepath.Join(tmpDir, "perf.json")
	if err := bridge.TraceStart(ctx, TraceStartOptions{BrowserID: "cdp-1"}); err != nil {
		t.Fatalf("TraceStart failed: %v", err)
	}
	summary, err := bridge.TraceStop(ctx, "cdp-1", tracePath)
	if err != nil {
		t.Fatalf("TraceStop failed: %v", err)
	}
	if summary.EventCount != 4 || summary.LongTaskCount != 1 || summary.LCPMS == nil || *summary.LCPMS != 450 {
		t.Fatalf("unexpected trace summary: %#v", summary)
	}
	if _, err := os.Stat(tracePath); err != nil {
		t.Fatalf("trace file missing: %v", err)
	}

	// Emulate + HeapSnapshot + Logpoint + RawCDP
	if err := bridge.Emulate(ctx, tabID, EmulateOptions{Width: 390, Height: 844, Mobile: true, NetworkPreset: "Fast 3G", ColorScheme: "dark"}); err != nil {
		t.Fatalf("Emulate failed: %v", err)
	}
	heapPath := filepath.Join(tmpDir, "mem.heapsnapshot")
	heapRes, err := bridge.HeapSnapshot(ctx, tabID, heapPath)
	if err != nil || heapRes.Bytes == 0 {
		t.Fatalf("HeapSnapshot failed: %#v err=%v", heapRes, err)
	}
	bpID, err := bridge.Logpoint(ctx, tabID, "https://example.com/app.js", 12, 0, "user.id")
	if err != nil || bpID != "bp-1" {
		t.Fatalf("Logpoint failed: %q err=%v", bpID, err)
	}
	rawRes, err := bridge.RawCDP(ctx, "cdp-1", "", "Browser.getVersion", nil)
	if err != nil || rawRes == nil {
		t.Fatalf("RawCDP failed: %#v err=%v", rawRes, err)
	}

	// CSS inspection must auto-initialize DOM.getDocument and include user-agent matched rules
	cssRes, err := bridge.CSS(ctx, tabID, Locator{Role: "button", Name: "Search", Exact: true})
	if err != nil {
		t.Fatalf("CSS failed on first call (expected auto DOM.getDocument): %v", err)
	}
	if cssRes.Computed["font-size"] != "16px" || len(cssRes.Matched) == 0 || cssRes.Matched[0].Origin != "user-agent" {
		t.Fatalf("unexpected CSS result: %#v", cssRes)
	}

	// Tab-scoped Tracing reads exact Web Performance API metrics alongside raw trace
	tabTracePath := filepath.Join(tmpDir, "perf-tab.json")
	if err := bridge.TraceStart(ctx, TraceStartOptions{BrowserID: "cdp-1", TabID: tabID, Reload: true, IgnoreCache: true}); err != nil {
		t.Fatalf("tab TraceStart failed: %v", err)
	}
	tabSummary, err := bridge.TraceStop(ctx, "cdp-1", tabTracePath)
	if err != nil {
		t.Fatalf("tab TraceStop failed: %v", err)
	}
	if tabSummary.MetricsSource != "page_performance_api" || tabSummary.DCLMS == nil || *tabSummary.DCLMS != 587.6 || tabSummary.LoadMS == nil || *tabSummary.LoadMS != 588.2 || tabSummary.CLS == nil || *tabSummary.CLS != 0.01 {
		t.Fatalf("expected page_performance_api metrics, got %#v", tabSummary)
	}

	// Concurrent Open (captures main Document request via about:blank -> attach -> Page.navigate) + MarkHandoff + CleanupSessionTabs
	tA, err := bridge.OpenWithOptions(ctx, OpenOptions{BrowserID: "cdp-1", URL: "https://a.test", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Open A failed: %v", err)
	}
	if tA.URL != "https://a.test" || tA.Title != "Opened https://a.test" {
		t.Fatalf("unexpected opened tab A metadata: %#v", tA)
	}
	aReqs, err := bridge.Network(ctx, tA.ID, NetworkFilter{})
	if err != nil {
		t.Fatalf("Network on opened tab A failed: %v", err)
	}
	foundMainDoc := false
	for _, r := range aReqs {
		if r.ResourceType == "document" && r.URL == "https://a.test" && r.Status == 200 {
			foundMainDoc = true
		}
	}
	if !foundMainDoc {
		t.Fatalf("expected main document request captured in tab.network() on Open, got %#v", aReqs)
	}
	tB, err := bridge.OpenWithOptions(ctx, OpenOptions{BrowserID: "cdp-1", URL: "https://b.test", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Open B failed: %v", err)
	}
	if err := bridge.MarkHandoff(tA.ID); err != nil {
		t.Fatalf("MarkHandoff failed: %v", err)
	}
	if closed := bridge.CleanupSessionTabs(ctx, "s1"); closed != 1 {
		t.Fatalf("expected 1 temp tab closed (B) and 1 handed-off tab kept (A), closed=%d", closed)
	}
	remaining, _ := bridge.Tabs(ctx, "cdp-1")
	for _, rt := range remaining {
		if rt.ID == tB.ID {
			t.Fatalf("temp tab B should have been closed")
		}
	}
	_ = http.StatusOK
}

func TestMyersLineDiffDetectsReorderAndNoOp(t *testing.T) {
	// Identical lines -> no changes.
	diff, added, removed := computeLineDiff([]string{"a", "b", "c"}, []string{"a", "b", "c"})
	if added != 0 || removed != 0 || !strings.Contains(diff, "no AX text changes") {
		t.Fatalf("expected no-op diff, got +%d/-%d %q", added, removed, diff)
	}
	// Pure reordering MUST be detected by Myers O(ND) diff (multiset bag diff would miss it).
	diff, added, removed = computeLineDiff([]string{"a", "b", "c"}, []string{"c", "b", "a"})
	if added == 0 || removed == 0 || !strings.Contains(diff, "[AX diff:") {
		t.Fatalf("Myers diff missed line reordering: +%d/-%d %q", added, removed, diff)
	}
}

func TestHiddenTabBackgroundCapturerLifecycle(t *testing.T) {
	var mu sync.Mutex
	var startCount, stopCount int

	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		sendJSON := func(v any) {
			b, _ := json.Marshal(v)
			_ = websocket.Message.Send(ws, string(b))
		}
		for {
			var raw string
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			var req struct {
				ID        int64           `json:"id"`
				Method    string          `json:"method"`
				Params    json.RawMessage `json:"params"`
				SessionID string          `json:"sessionId"`
			}
			if json.Unmarshal([]byte(raw), &req) != nil {
				continue
			}
			var params map[string]any
			_ = json.Unmarshal(req.Params, &params)

			switch req.Method {
			case "Browser.getVersion":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"product": "Edge/158"}})
			case "Target.attachToTarget":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"sessionId": "S_HIDDEN"}})
			case "Target.getTargets":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{
					"targetInfos": []map[string]any{{"targetId": "T_BG", "type": "page", "title": "BG", "url": "https://bg.test"}},
				}})
			case "Runtime.evaluate":
				expr, _ := params["expression"].(string)
				if expr == "document.hidden === true" {
					sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": true}}})
				} else {
					sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "ok"}}})
				}
			case "Page.startScreencast":
				mu.Lock()
				startCount++
				mu.Unlock()
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
			case "Page.stopScreencast":
				mu.Lock()
				stopCount++
				mu.Unlock()
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
			case "Page.getLayoutMetrics":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"visualViewport": map[string]any{"clientWidth": 1200, "clientHeight": 800}}})
			case "Page.captureScreenshot":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("bgpng"))}})
			case "DOM.getBoxModel":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"model": map[string]any{"content": []float64{10, 10, 50, 10, 50, 30, 10, 30}}}})
			case "Accessibility.getFullAXTree":
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{"nodes": []map[string]any{
					{"role": map[string]any{"value": "button"}, "name": map[string]any{"value": "Go"}, "backendDOMNodeId": 7},
				}}})
			default:
				sendJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bridge, err := Start(ctx, config.Browser{DisableAutoCDP: true, CDPEndpoints: []string{wsURL}})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close(context.Background())

	tabID := "cdp-1:T_BG"
	// Consecutive click + screenshot + click on hidden tab must start capturer ONCE and reuse it.
	if _, err := bridge.Act(ctx, Action{TabID: tabID, Kind: "click", Locator: Locator{Role: "button", Name: "Go"}}); err != nil {
		t.Fatalf("hidden tab click 1 failed: %v", err)
	}
	data, info, err := bridge.Screenshot(ctx, ScreenshotOptions{TabID: tabID})
	if err != nil || string(data) != "bgpng" || info.Width != 1200 || info.Height != 800 {
		t.Fatalf("hidden tab screenshot failed: len=%d info=%+v err=%v", len(data), info, err)
	}
	if _, err := bridge.Act(ctx, Action{TabID: tabID, Kind: "click", Locator: Locator{Role: "button", Name: "Go"}}); err != nil {
		t.Fatalf("hidden tab click 2 failed: %v", err)
	}
	if err := bridge.MarkHandoff(tabID); err != nil {
		t.Fatalf("MarkHandoff failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if startCount != 1 || stopCount != 1 {
		t.Fatalf("expected exactly 1 startScreencast and 1 stopScreencast on handoff, got start=%d stop=%d", startCount, stopCount)
	}
}
