package runtimehost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
)

var errDiagnosticResponseLost = errors.New("diagnostic: deliberately discarded process_continue response")

// diagnosticResponseLoss completes the real HTTP request before withholding its
// response from the MCP client. This models response loss after a handler has
// run, rather than cancellation before the process result has been collected.
type diagnosticResponseLoss struct {
	mu            sync.Mutex
	dropNext      bool
	continueCalls int
	dropped       []runtimeprocess.Result
}

func (r *diagnosticResponseLoss) arm() {
	r.mu.Lock()
	r.dropNext = true
	r.mu.Unlock()
}

func (r *diagnosticResponseLoss) RoundTrip(req *http.Request) (*http.Response, error) {
	var message struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		err = json.NewDecoder(body).Decode(&message)
		_ = body.Close()
		if err != nil {
			return nil, err
		}
	}
	drop := false
	if message.Method == "tools/call" && message.Params.Name == "process_continue" {
		r.mu.Lock()
		r.continueCalls++
		drop, r.dropNext = r.dropNext, false
		r.mu.Unlock()
	}
	response, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !drop {
		return response, err
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	var captured struct {
		Result struct {
			StructuredContent runtimeprocess.Result `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &captured); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.dropped = append(r.dropped, captured.Result.StructuredContent)
	r.mu.Unlock()
	return nil, errDiagnosticResponseLost
}

// Fault injection verifies cursor recovery after a response has been produced.
// It is not evidence that a production client experienced this injected failure.
func TestHTTPContinueResponseLossCanRecoverRunningAndCompletedOutput(t *testing.T) {
	directory := t.TempDir()
	loss := &diagnosticResponseLoss{}
	ctx, a, b := sharedHTTPTestClients(t, loss, http.DefaultTransport)
	clients := []struct {
		label     string
		sessionID string
		cursor    runtimeprocess.OutputCursor
	}{{label: "A"}, {label: "B"}}
	for index, client := range clients {
		session := a
		if index == 1 {
			session = b
		}
		started, err := callProcessTool(ctx, session, "process_run", map[string]any{
			"program": os.Args[0], "args": []string{"-test.run=^TestHTTPDeliveryProcessHelper$", "--", client.label, directory},
			"environment":   map[string]string{"LOCAL_RUNTIME_MCP_HTTP_DELIVERY_HELPER": "1", "GORACE": "atexit_sleep_ms=0"},
			"yield_time_ms": 1, "timeout_seconds": 20,
		})
		if err != nil || !started.Running || started.SessionID == "" || started.Stdout != "" {
			t.Fatalf("launch %s: %+v, %v", client.label, started, err)
		}
		clients[index].sessionID = started.SessionID
		clients[index].cursor = started.OutputCursor
		waitForProcessHelperFile(t, ctx, filepath.Join(directory, client.label+".ready"), nil)
		if err := os.WriteFile(filepath.Join(directory, client.label+".output"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		waitForProcessHelperFile(t, ctx, filepath.Join(directory, client.label+".emitted"), nil)
	}

	loss.arm()
	if _, err := callProcessTool(ctx, a, "process_continue", map[string]any{"session_id": clients[0].sessionID, "output_cursor": clients[0].cursor, "yield_time_ms": 1000}); !errors.Is(err, errDiagnosticResponseLost) {
		t.Fatalf("expected injected response loss, got %v", err)
	}
	// This is a read-only retry; stdin, termination, and PTY operations must not
	// be replayed simply because their HTTP response was lost.
	aRetry, err := callProcessTool(ctx, a, "process_continue", map[string]any{"session_id": clients[0].sessionID, "output_cursor": clients[0].cursor, "yield_time_ms": 1})
	if err != nil || !aRetry.Running || strings.TrimSpace(aRetry.Stdout) != "A early" || aRetry.Stderr != "" {
		t.Fatalf("running retry: %+v, %v", aRetry, err)
	}
	bRunning, err := callProcessTool(ctx, b, "process_continue", map[string]any{"session_id": clients[1].sessionID, "output_cursor": clients[1].cursor, "yield_time_ms": 1000})
	if err != nil || !bRunning.Running || strings.TrimSpace(bRunning.Stdout) != "B early" || bRunning.Stderr != "" {
		t.Fatalf("independent B running result: %+v, %v", bRunning, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "A.finish"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	loss.arm()
	if _, err := callProcessTool(ctx, a, "process_continue", map[string]any{"session_id": clients[0].sessionID, "output_cursor": aRetry.OutputCursor, "yield_time_ms": 6000}); !errors.Is(err, errDiagnosticResponseLost) {
		t.Fatalf("expected completed response loss, got %v", err)
	}
	aCompleted, err := callProcessTool(ctx, a, "process_continue", map[string]any{"session_id": clients[0].sessionID, "output_cursor": aRetry.OutputCursor, "yield_time_ms": 1})
	if err != nil || aCompleted.Running || aCompleted.ExitCode != 0 || strings.TrimSpace(aCompleted.Stdout) != "A final" || aCompleted.Stderr != "" {
		t.Fatalf("completed retry failed to recover output: %+v, %v", aCompleted, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "B.finish"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bCompleted, err := callProcessTool(ctx, b, "process_continue", map[string]any{"session_id": clients[1].sessionID, "output_cursor": bRunning.OutputCursor, "yield_time_ms": 6000})
	if err != nil || bCompleted.Running || bCompleted.ExitCode != 0 || strings.TrimSpace(bCompleted.Stdout) != "B final" || bCompleted.Stderr != "" {
		t.Fatalf("independent B completed result: %+v, %v", bCompleted, err)
	}
	loss.mu.Lock()
	defer loss.mu.Unlock()
	if loss.continueCalls != 4 || len(loss.dropped) != 2 {
		t.Fatalf("unexpected automatic POST retry: calls=%d dropped=%d", loss.continueCalls, len(loss.dropped))
	}
	running, completed := loss.dropped[0], loss.dropped[1]
	if !running.Running || running.SessionID != clients[0].sessionID || strings.TrimSpace(running.Stdout) != "A early" {
		t.Fatalf("discarded running response: %+v", running)
	}
	if completed.Running || completed.ExitCode != 0 || strings.TrimSpace(completed.Stdout) != "A final" {
		t.Fatalf("discarded completed response: %+v", completed)
	}
	if running.OutputCursor != aRetry.OutputCursor || running.StdoutOffset != aRetry.StdoutOffset || completed.OutputCursor != aCompleted.OutputCursor || completed.DurationMS != aCompleted.DurationMS {
		t.Fatalf("cursor recovery changed output positions or final duration: running=%+v retry=%+v completed=%+v retry=%+v", running, aRetry, completed, aCompleted)
	}
	t.Logf("A running response contained %q; same-cursor read-only retry recovered %q", strings.TrimSpace(running.Stdout), strings.TrimSpace(aRetry.Stdout))
	t.Logf("A completed response contained %q; same-cursor read-only retry recovered %q with duration=%dms", strings.TrimSpace(completed.Stdout), strings.TrimSpace(aCompleted.Stdout), aCompleted.DurationMS)
	t.Logf("B received %q and %q; A issued exactly %d continue POSTs without automatic retry", strings.TrimSpace(bRunning.Stdout), strings.TrimSpace(bCompleted.Stdout), loss.continueCalls)
}

func TestHTTPDeliveryProcessHelper(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_HTTP_DELIVERY_HELPER") != "1" {
		return
	}
	label, directory := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	publish := func(suffix string) {
		if err := os.WriteFile(filepath.Join(directory, label+suffix), nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	wait := func(suffix string) {
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(directory, label+suffix)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "delivery helper barrier timed out:", suffix)
				os.Exit(2)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	publish(".ready")
	wait(".output")
	fmt.Println(label, "early")
	publish(".emitted")
	wait(".finish")
	fmt.Println(label, "final")
	os.Exit(0)
}
