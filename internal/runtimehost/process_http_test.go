package runtimehost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolIDRecorder struct {
	mu  sync.Mutex
	ids map[string][]string
}

func (r *toolIDRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		decodeErr := json.NewDecoder(body).Decode(&message)
		_ = body.Close()
		if decodeErr == nil && message.Method == "tools/call" {
			r.mu.Lock()
			r.ids[message.Params.Name] = append(r.ids[message.Params.Name], string(message.ID))
			r.mu.Unlock()
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (r *toolIDRecorder) firstID(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ids := r.ids[name]; len(ids) > 0 {
		return ids[0]
	}
	return ""
}

type processHelperTiming struct {
	StartedNS  int64 `json:"started_ns"`
	FinishedNS int64 `json:"finished_ns"`
}

func TestHTTPProcessSessionsOverlapAndDurationExcludesCollectionDelay(t *testing.T) {
	directory := t.TempDir()
	recorders := []*toolIDRecorder{{ids: make(map[string][]string)}, {ids: make(map[string][]string)}}
	ctx, a, b := sharedHTTPTestClients(t, recorders[0], recorders[1])
	clients := []*mcp.ClientSession{a, b}
	type launch struct {
		index     int
		requested time.Time
		result    runtimeprocess.Result
		err       error
	}
	launches := make(chan launch, 2)
	start := make(chan struct{})
	for index, client := range clients {
		go func(index int, client *mcp.ClientSession) {
			<-start
			requested := time.Now()
			result, err := callProcessTool(ctx, client, "process_run", map[string]any{
				"program": os.Args[0], "args": []string{"-test.run=^TestRuntimeProcessHelper$", "--", []string{"GPT-A", "GPT-B"}[index], directory},
				"environment":   map[string]string{"LOCAL_RUNTIME_MCP_RUNTIME_PROCESS_HELPER": "1", "GORACE": "atexit_sleep_ms=0"},
				"yield_time_ms": 1, "timeout_seconds": 10,
			})
			launches <- launch{index, requested, result, err}
		}(index, client)
	}
	close(start)
	started := make([]launch, 2)
	for range 2 {
		value := <-launches
		if value.err != nil || !value.result.Running || value.result.SessionID == "" {
			t.Fatalf("launch %d: %+v, %v", value.index, value.result, value.err)
		}
		started[value.index] = value
	}
	if started[0].result.SessionID == started[1].result.SessionID {
		t.Fatal("independent processes received the same session_id")
	}
	if first, second := recorders[0].firstID("process_run"), recorders[1].firstID("process_run"); first == "" || first != second {
		t.Fatalf("expected independently reused process_run JSON-RPC IDs: %q/%q", first, second)
	}
	for _, label := range []string{"GPT-A", "GPT-B"} {
		waitForProcessHelperFile(t, ctx, filepath.Join(directory, label+".ready"), nil)
	}
	// Both processes are alive and blocked on this one barrier. Release only
	// after both ready markers exist, rather than relying on scheduler timing.
	if err := os.WriteFile(filepath.Join(directory, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	timings := make([]processHelperTiming, 2)
	for index, label := range []string{"GPT-A", "GPT-B"} {
		waitForProcessHelperFile(t, ctx, filepath.Join(directory, label+".done"), &timings[index])
	}
	if timings[0].StartedNS >= timings[1].FinishedNS || timings[1].StartedNS >= timings[0].FinishedNS {
		t.Fatalf("process lifetimes did not overlap: %+v", timings)
	}
	// Collection intentionally occurs well after both helpers complete. This
	// client delay must not become part of either process duration.
	time.Sleep(2 * time.Second)
	for index, client := range clients {
		go func(index int, client *mcp.ClientSession) {
			completed, err := callProcessTool(ctx, client, "process_continue", map[string]any{
				"session_id": started[index].result.SessionID, "output_cursor": started[index].result.OutputCursor, "yield_time_ms": 1000,
			})
			launches <- launch{index: index, result: completed, err: err}
		}(index, client)
	}
	for range 2 {
		value := <-launches
		index, completed := value.index, value.result
		if value.err != nil || completed.Running || completed.ExitCode != 0 || completed.Stderr != "" {
			t.Fatalf("collection %d: %+v, %v", index, completed, value.err)
		}
		label := []string{"GPT-A", "GPT-B"}[index]
		output := started[index].result.Stdout + completed.Stdout
		want := fmt.Sprintf("%s tick=1\n%s tick=2\n%s tick=3\n", label, label, label)
		if strings.ReplaceAll(output, "\r\n", "\n") != want {
			t.Fatalf("session output was lost or mixed: want=%q got=%q", want, output)
		}
		// Request-to-helper-finish is an upper bound that includes HTTP and
		// launch overhead. Allow 1s for exit/reaping on loaded CI machines,
		// but not the deliberate 2s delay. Helpers disable race exit sleeping.
		upperBound := time.Unix(0, timings[index].FinishedNS).Sub(started[index].requested).Milliseconds() + 1000
		if completed.DurationMS < started[index].result.DurationMS || completed.DurationMS > upperBound {
			t.Fatalf("duration includes collection delay: duration=%dms upper_bound=%dms", completed.DurationMS, upperBound)
		}
		t.Logf("%s session=%s interval=%d..%d duration=%dms", label, started[index].result.SessionID, timings[index].StartedNS, timings[index].FinishedNS, completed.DurationMS)
	}
	if first, second := recorders[0].firstID("process_continue"), recorders[1].firstID("process_continue"); first == "" || first != second {
		t.Fatalf("expected independently reused continue JSON-RPC IDs: %q/%q", first, second)
	}
}

func callProcessTool(ctx context.Context, client *mcp.ClientSession, name string, arguments any) (runtimeprocess.Result, error) {
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return runtimeprocess.Result{}, err
	}
	if result.IsError {
		return runtimeprocess.Result{}, &toolFailure{result}
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return runtimeprocess.Result{}, err
	}
	var output runtimeprocess.Result
	err = json.Unmarshal(data, &output)
	return output, err
}

func waitForProcessHelperFile(t *testing.T, ctx context.Context, path string, timing *processHelperTiming) {
	t.Helper()
	for {
		data, err := os.ReadFile(path)
		if err == nil && (timing == nil || json.Unmarshal(data, timing) == nil) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("helper did not publish %s: %v", filepath.Base(path), ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRuntimeProcessHelper(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_RUNTIME_PROCESS_HELPER") != "1" {
		return
	}
	label, directory := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	timing := processHelperTiming{StartedNS: time.Now().UnixNano()}
	if err := os.WriteFile(filepath.Join(directory, label+".ready"), nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(directory, "release")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "helper release barrier timed out")
			os.Exit(2)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for tick := 1; tick <= 3; tick++ {
		fmt.Printf("%s tick=%d\n", label, tick)
		time.Sleep(10 * time.Millisecond)
	}
	timing.FinishedNS = time.Now().UnixNano()
	data, _ := json.Marshal(timing)
	if err := os.WriteFile(filepath.Join(directory, label+".done"), data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}
