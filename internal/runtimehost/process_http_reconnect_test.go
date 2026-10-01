package runtimehost

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type interruptedProcessResponse struct {
	mu       sync.Mutex
	endpoint string
	armed    bool
}

func (r *interruptedProcessResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.endpoint = req.URL.String()
	interrupt := r.armed && req.Header.Get("Mcp-Method") == "tools/call"
	if interrupt {
		r.armed = false
	}
	r.mu.Unlock()
	response, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && interrupt {
		response.Body = &interruptedProcessBody{ReadCloser: response.Body, remaining: 16}
	}
	return response, err
}

type interruptedProcessBody struct {
	io.ReadCloser
	remaining int
}

func (b *interruptedProcessBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, err
}

// A client session may be unusable after an incomplete JSON response. Process
// ownership belongs to the Host, so a new connection can still use its known
// session ID and last received cursor without launching or replaying input.
func TestHTTPProcessOutputSurvivesClientReconnectAfterTruncatedResponse(t *testing.T) {
	directory := t.TempDir()
	fault := &interruptedProcessResponse{}
	ctx, a, b := sharedHTTPTestClients(t, fault, http.DefaultTransport)
	started, err := callProcessTool(ctx, a, "process_run", map[string]any{
		"program": os.Args[0], "args": []string{"-test.run=^TestHTTPDeliveryProcessHelper$", "--", "reconnect-A", directory},
		"environment":   map[string]string{"LOCAL_RUNTIME_MCP_HTTP_DELIVERY_HELPER": "1", "GORACE": "atexit_sleep_ms=0"},
		"yield_time_ms": 1, "timeout_seconds": 20,
	})
	if err != nil || !started.Running || started.SessionID == "" || started.Stdout != "" {
		t.Fatalf("launch: %+v, %v", started, err)
	}
	waitForProcessHelperFile(t, ctx, filepath.Join(directory, "reconnect-A.ready"), nil)
	if err := os.WriteFile(filepath.Join(directory, "reconnect-A.output"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForProcessHelperFile(t, ctx, filepath.Join(directory, "reconnect-A.emitted"), nil)
	fault.mu.Lock()
	fault.armed = true
	fault.mu.Unlock()
	arguments := map[string]any{"session_id": started.SessionID, "output_cursor": started.OutputCursor, "yield_time_ms": 1000}
	if _, err := callProcessTool(ctx, a, "process_continue", arguments); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("expected incomplete response, got %v", err)
	}
	if _, err := a.ListTools(ctx, nil); err == nil {
		t.Fatal("original SDK client session unexpectedly remained usable")
	}
	if result, err := b.CallTool(ctx, &mcp.CallToolParams{Name: "filesystem_stat", Arguments: map[string]any{"path": directory}}); err != nil || result.IsError {
		t.Fatalf("independent client failed: %+v, %v", result, err)
	}
	fault.mu.Lock()
	endpoint := fault.endpoint
	fault.mu.Unlock()
	a2, err := mcp.NewClient(&mcp.Implementation{Name: "reconnected-process-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: authorization{http.DefaultTransport, "0123456789abcdef0123456789abcdef"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close() })
	recovered, err := callProcessTool(ctx, a2, "process_continue", arguments)
	if err != nil || !recovered.Running || strings.TrimSpace(recovered.Stdout) != "reconnect-A early" {
		t.Fatalf("known session and cursor did not survive reconnect: %+v, %v", recovered, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "reconnect-A.finish"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	arguments["output_cursor"], arguments["yield_time_ms"] = recovered.OutputCursor, 6000
	completed, err := callProcessTool(ctx, a2, "process_continue", arguments)
	if err != nil || completed.Running || completed.ExitCode != 0 || strings.TrimSpace(completed.Stdout) != "reconnect-A final" {
		t.Fatalf("completed result after reconnect: %+v, %v", completed, err)
	}
	arguments["output_cursor"] = completed.OutputCursor
	final, err := callProcessTool(ctx, a2, "process_continue", arguments)
	if err != nil || final.Running || final.Stdout != "" || final.Stderr != "" || final.DurationMS != completed.DurationMS {
		t.Fatalf("final result did not remain readable: %+v, %v", final, err)
	}
}
