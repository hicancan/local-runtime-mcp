package runtimehost

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/jsrt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHTTPParallelRunAndAsyncWait(t *testing.T) {
	ctx, a, b := sharedHTTPTestClients(t, http.DefaultTransport, http.DefaultTransport)

	// Single turn parallel fan-out via Promise.allSettled over HTTP
	start := time.Now()
	res, err := a.CallTool(ctx, &mcp.CallToolParams{
		Name: "run",
		Arguments: map[string]any{
			"title":      "HTTP parallel sleep fanout",
			"done_title": "HTTP parallel fanout completed",
			"code": `
				const out = await Promise.allSettled([sleep(120), sleep(120), sleep(120)]);
				return out.map(r => r.status);
			`,
		},
	})
	elapsed := time.Since(start)
	if err != nil || res.IsError {
		t.Fatalf("parallel HTTP run failed: %v %+v", err, res)
	}
	if elapsed > 320*time.Millisecond {
		t.Fatalf("expected concurrent execution (<320ms), took %v", elapsed)
	}

	// Async cell started by client A, collected via wait by client B
	r1, err := a.CallTool(ctx, &mcp.CallToolParams{
		Name: "run",
		Arguments: map[string]any{
			"title":    "Async background cell",
			"yield_ms": 40,
			"code":     `print("tick1"); await sleep(150); print("tick2"); return "done-over-http";`,
		},
	})
	if err != nil || r1.IsError {
		t.Fatalf("start async cell failed: %v %+v", err, r1)
	}
	var c1 jsrt.CellResult
	b1, _ := json.Marshal(r1.StructuredContent)
	_ = json.Unmarshal(b1, &c1)
	if c1.Status != "running" || c1.CellID == "" {
		t.Fatalf("expected running cell, got %+v", c1)
	}

	r2, err := b.CallTool(ctx, &mcp.CallToolParams{
		Name:      "wait",
		Arguments: map[string]any{"cell_id": c1.CellID, "yield_ms": 3000},
	})
	if err != nil || r2.IsError {
		t.Fatalf("wait over HTTP failed: %v %+v", err, r2)
	}
	var c2 jsrt.CellResult
	b2, _ := json.Marshal(r2.StructuredContent)
	_ = json.Unmarshal(b2, &c2)
	if c2.Status != "done" || c2.Value != "done-over-http" || !strings.Contains(c2.Output, "tick2") {
		t.Fatalf("unexpected wait result: %+v", c2)
	}
}
