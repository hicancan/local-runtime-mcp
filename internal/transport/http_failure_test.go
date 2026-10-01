package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type httpFailureInput struct {
	Label string `json:"label"`
	Wait  bool   `json:"wait,omitempty"`
}

type httpFailureOutput struct {
	Label string `json:"label"`
}

func startHTTPFailureServer(t *testing.T, probe func(context.Context, httpFailureInput) (httpFailureOutput, error)) (context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	server := mcp.NewServer(&mcp.Implementation{Name: "http-failure-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "probe", Description: "Return a test label."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in httpFailureInput) (*mcp.CallToolResult, httpFailureOutput, error) {
			out, err := probe(ctx, in)
			return nil, out, err
		})
	instance, err := NewHTTP(server, HTTPConfig{Listen: "127.0.0.1:0", BearerToken: testBearerToken})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("HTTP server shutdown: %v", err)
		}
	})
	return ctx, "http://" + instance.Address() + "/mcp"
}

func connectHTTPFailureClient(t *testing.T, ctx context.Context, endpoint string, base http.RoundTripper) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-failure-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{base: base, token: testBearerToken}},
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callHTTPFailureProbe(ctx context.Context, session *mcp.ClientSession, label string, wait bool) (string, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "probe", Arguments: httpFailureInput{Label: label, Wait: wait},
	})
	if err != nil {
		return "", err
	}
	if result.IsError {
		return "", fmt.Errorf("probe returned a tool error: %+v", result.Content)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return "", err
	}
	var out httpFailureOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.Label, nil
}

func assertHTTPFailureProbe(t *testing.T, ctx context.Context, session *mcp.ClientSession, label string) {
	t.Helper()
	if got, err := callHTTPFailureProbe(ctx, session, label, false); err != nil || got != label {
		t.Fatalf("probe %q: label=%q err=%v", label, got, err)
	}
}

func awaitHTTPFailure[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("test barrier timed out: %v", ctx.Err())
		var zero T
		return zero
	}
}

// Raw HTTP makes the cancellation carrier explicit, without SDK client
// notifications or automatic recovery. In legacy stateless mode the SDK
// intentionally detaches tool contexts; request cancellation is propagated
// only for the 2026-07-28 protocol. Neither path should affect another POST.
func TestHTTPRawCancellationProtocolVersionsIsolation(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			started, cancelled, finished, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			ctx, endpoint := startHTTPFailureServer(t, func(ctx context.Context, in httpFailureInput) (httpFailureOutput, error) {
				if in.Wait {
					close(started)
					defer close(finished)
					select {
					case <-ctx.Done():
						close(cancelled)
						return httpFailureOutput{}, ctx.Err()
					case <-release:
					}
				}
				return httpFailureOutput{Label: in.Label}, nil
			})
			defer releaseOnce.Do(func() { close(release) })
			aCtx, cancelA := context.WithCancel(ctx)
			defer cancelA()
			aDone := make(chan error, 1)
			go func() {
				_, err := rawHTTPFailureProbe(aCtx, endpoint, version, "A", true)
				aDone <- err
			}()
			awaitHTTPFailure(t, ctx, started)
			cancelA()
			if err := awaitHTTPFailure(t, ctx, aDone); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled HTTP call: %v", err)
			}
			// Both raw requests deliberately use JSON-RPC ID 77.
			if got, err := rawHTTPFailureProbe(ctx, endpoint, version, "B", false); err != nil || got != "B" {
				t.Fatalf("independent POST after A cancellation: label=%q err=%v", got, err)
			}
			if version == "2026-07-28" {
				awaitHTTPFailure(t, ctx, cancelled)
			} else {
				select {
				case <-cancelled:
					t.Fatal("legacy stateless handler unexpectedly inherited HTTP cancellation")
				case <-time.After(100 * time.Millisecond):
				}
			}
			releaseOnce.Do(func() { close(release) })
			awaitHTTPFailure(t, ctx, finished)
			if got, err := rawHTTPFailureProbe(ctx, endpoint, version, "A2", false); err != nil || got != "A2" {
				t.Fatalf("fresh POST after A finishes: label=%q err=%v", got, err)
			}
		})
	}
}

func rawHTTPFailureProbe(ctx context.Context, endpoint, version, label string, wait bool) (string, error) {
	params := map[string]any{"name": "probe", "arguments": httpFailureInput{Label: label, Wait: wait}}
	if version == "2026-07-28" {
		params["_meta"] = map[string]any{
			mcp.MetaKeyProtocolVersion:    version,
			mcp.MetaKeyClientInfo:         map[string]any{"name": "raw-failure-client", "version": "1"},
			mcp.MetaKeyClientCapabilities: map[string]any{},
		}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 77, "method": "tools/call", "params": params})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+testBearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", version)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "probe")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Mcp-Session-Id") != "" {
		return "", errors.New("stateless response unexpectedly assigned a session ID")
	}
	var rpc struct {
		ID     int             `json:"id"`
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError           bool              `json:"isError"`
			StructuredContent httpFailureOutput `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return "", err
	}
	if rpc.ID != 77 || len(rpc.Error) != 0 || rpc.Result.IsError {
		return "", fmt.Errorf("unexpected JSON-RPC response: %+v", rpc)
	}
	return rpc.Result.StructuredContent.Label, nil
}

type httpFailureRoundTripFunc func(*http.Request) (*http.Response, error)

func (f httpFailureRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHTTPClientFailureBeforeHeadersIsIsolated(t *testing.T) {
	var executions atomic.Int32
	ctx, endpoint := startHTTPFailureServer(t, func(_ context.Context, in httpFailureInput) (httpFailureOutput, error) {
		executions.Add(1)
		return httpFailureOutput{Label: in.Label}, nil
	})
	injected := errors.New("injected connection failure before response headers")
	var fired atomic.Bool
	fault := httpFailureRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Mcp-Method") == "tools/call" && fired.CompareAndSwap(false, true) {
			return nil, injected
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	a := connectHTTPFailureClient(t, ctx, endpoint, fault)
	b := connectHTTPFailureClient(t, ctx, endpoint, http.DefaultTransport)
	if _, err := callHTTPFailureProbe(ctx, a, "A-failed", false); !errors.Is(err, injected) {
		t.Fatalf("injected request failure: %v", err)
	}
	if got := executions.Load(); got != 0 {
		t.Fatalf("failed request unexpectedly executed the tool: %d", got)
	}
	// SDK v1.8.0 treats this as a per-call rejection, so A remains usable.
	assertHTTPFailureProbe(t, ctx, a, "A-after")
	assertHTTPFailureProbe(t, ctx, b, "B")
}

type httpFailureTruncatedBody struct {
	io.ReadCloser
	remaining int
}

func (b *httpFailureTruncatedBody) Read(p []byte) (int, error) {
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

func TestHTTPClientTruncatedJSONIsIsolatedAndNewClientReconnects(t *testing.T) {
	var executions atomic.Int32
	ctx, endpoint := startHTTPFailureServer(t, func(_ context.Context, in httpFailureInput) (httpFailureOutput, error) {
		executions.Add(1)
		return httpFailureOutput{Label: in.Label}, nil
	})
	var fired atomic.Bool
	fault := httpFailureRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err == nil && req.Header.Get("Mcp-Method") == "tools/call" && fired.CompareAndSwap(false, true) {
			resp.Body = &httpFailureTruncatedBody{ReadCloser: resp.Body, remaining: 16}
		}
		return resp, err
	})
	a := connectHTTPFailureClient(t, ctx, endpoint, fault)
	b := connectHTTPFailureClient(t, ctx, endpoint, http.DefaultTransport)
	if _, err := callHTTPFailureProbe(ctx, a, "A-failed", false); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("truncated JSON response: %v", err)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("tool should have executed once before its response was truncated: %d", got)
	}
	// A JSON body read failure is terminal for this SDK client session. Do not
	// mistake a successful new connection for recovery of the old session.
	if _, err := a.ListTools(ctx, nil); err == nil {
		t.Fatal("truncated response did not terminate the original SDK client session")
	}
	assertHTTPFailureProbe(t, ctx, b, "B")
	a2 := connectHTTPFailureClient(t, ctx, endpoint, fault)
	assertHTTPFailureProbe(t, ctx, a2, "A2")
	if got := executions.Load(); got != 3 {
		t.Fatalf("failed non-idempotent call was unexpectedly replayed: executions=%d", got)
	}
}

type httpFailureIDRecorder struct {
	mu  sync.Mutex
	ids map[string]string
}

func (r *httpFailureIDRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.GetBody != nil && req.Header.Get("Mcp-Method") == "tools/call" {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Arguments httpFailureInput `json:"arguments"`
			} `json:"params"`
		}
		err = json.NewDecoder(body).Decode(&msg)
		_ = body.Close()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.ids[msg.Params.Arguments.Label] = string(msg.ID)
		r.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (r *httpFailureIDRecorder) id(label string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ids[label]
}

func TestHTTPConcurrentCallsOutOfOrderIsolation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	ctx, endpoint := startHTTPFailureServer(t, func(ctx context.Context, in httpFailureInput) (httpFailureOutput, error) {
		if in.Wait {
			close(started)
			select {
			case <-ctx.Done():
				return httpFailureOutput{}, ctx.Err()
			case <-release:
			}
		}
		return httpFailureOutput{Label: in.Label}, nil
	})
	defer releaseOnce.Do(func() { close(release) })
	recorderA, recorderB := &httpFailureIDRecorder{ids: make(map[string]string)}, &httpFailureIDRecorder{ids: make(map[string]string)}
	a := connectHTTPFailureClient(t, ctx, endpoint, recorderA)
	b := connectHTTPFailureClient(t, ctx, endpoint, recorderB)
	type outcome struct {
		label string
		err   error
	}
	slowDone := make(chan outcome, 1)
	go func() {
		label, err := callHTTPFailureProbe(ctx, a, "A-slow", true)
		slowDone <- outcome{label, err}
	}()
	awaitHTTPFailure(t, ctx, started)
	fastDone := make(chan outcome, 1)
	bDone := make(chan outcome, 1)
	go func() {
		label, err := callHTTPFailureProbe(ctx, a, "A-fast", false)
		fastDone <- outcome{label, err}
	}()
	go func() {
		label, err := callHTTPFailureProbe(ctx, b, "B", false)
		bDone <- outcome{label, err}
	}()
	if got := awaitHTTPFailure(t, ctx, fastDone); got.err != nil || got.label != "A-fast" {
		t.Fatalf("same-client fast response: %+v", got)
	}
	if got := awaitHTTPFailure(t, ctx, bDone); got.err != nil || got.label != "B" {
		t.Fatalf("independent client response: %+v", got)
	}
	select {
	case got := <-slowDone:
		t.Fatalf("slow response completed before its release barrier: %+v", got)
	default:
	}
	if slowID, bID := recorderA.id("A-slow"), recorderB.id("B"); slowID == "" || slowID != bID {
		t.Fatalf("expected independent clients to reuse a JSON-RPC ID: %q/%q", slowID, bID)
	}
	if fastID, slowID := recorderA.id("A-fast"), recorderA.id("A-slow"); fastID == "" || fastID == slowID {
		t.Fatalf("same-client outstanding requests should have distinct IDs: %q/%q", fastID, slowID)
	}
	releaseOnce.Do(func() { close(release) })
	if got := awaitHTTPFailure(t, ctx, slowDone); got.err != nil || got.label != "A-slow" {
		t.Fatalf("same-client delayed response: %+v", got)
	}
	assertHTTPFailureProbe(t, ctx, a, "A-after")
	assertHTTPFailureProbe(t, ctx, b, "B-after")
}
