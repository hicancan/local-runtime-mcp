package runtimehost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type multiIngressPrivateHop struct {
	base  http.RoundTripper
	token string
}

func (h multiIngressPrivateHop) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set(transport.InternalTokenHeader, h.token)
	// Provider forwarding preserves external metadata. This deliberately uses
	// an unrelated external credential; the private hop authenticates its own.
	clone.Header.Set("Authorization", "Bearer forwarded-client-credential")
	clone.Header.Set("Origin", "https://chatgpt.com")
	return h.base.RoundTrip(clone)
}

// Exercise two actual HTTP listeners, not two clients of one listener. Their
// authentication and lifetimes differ, while process identity belongs to the
// one Host. The test launches only its own Go test helper, with owned fixtures.
func TestMultiIngressHostSharesSessionsAndSurvivesPrivateClosure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	bridgeToken := multiIngressToken(t)
	host, err := Start(ctx, &config.Config{Browser: config.Browser{Listen: "127.0.0.1:0", Token: bridgeToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := host.Close(shutdown); err != nil {
			t.Errorf("close shared Host: %v", err)
		}
	})

	privateToken, publicToken := multiIngressToken(t), multiIngressToken(t)
	privateOrigin, stopPrivate := multiIngressHTTP(t, ctx, host, transport.HTTPConfig{
		Listen: "127.0.0.1:0", BearerToken: privateToken, InternalToken: true,
	})
	publicOrigin, _ := multiIngressHTTP(t, ctx, host, transport.HTTPConfig{
		Listen: "127.0.0.1:0", BearerToken: publicToken,
	})
	if privateOrigin == publicOrigin {
		t.Fatal("independent entrances reused the same listener address")
	}
	recorderA := &toolIDRecorder{ids: make(map[string][]string)}
	recorderB := &toolIDRecorder{ids: make(map[string][]string)}
	a := multiIngressConnect(t, ctx, privateOrigin, multiIngressPrivateHop{recorderA, privateToken}, "private-client")
	b := multiIngressConnect(t, ctx, publicOrigin, authorization{recorderB, publicToken}, "public-client")
	for _, session := range []*mcp.ClientSession{a, b} {
		identity := session.InitializeResult().ServerInfo
		if identity.Name != "local-runtime-mcp" || identity.Version != mcpserver.Version {
			t.Fatalf("unexpected shared Server identity: %+v", identity)
		}
		catalog, err := session.ListTools(ctx, nil)
		if err != nil || len(catalog.Tools) != 21 {
			t.Fatalf("entrance catalog: result=%+v err=%v", catalog, err)
		}
	}

	directory := t.TempDir()
	labels := []string{"private-A", "public-B"}
	type launch struct {
		index  int
		result runtimeprocess.Result
		err    error
	}
	launched := make(chan launch, 2)
	barrier := make(chan struct{})
	for index, client := range []*mcp.ClientSession{a, b} {
		go func(index int, client *mcp.ClientSession) {
			<-barrier
			result, err := callProcessTool(ctx, client, "process_run", map[string]any{
				"program": os.Args[0], "args": []string{"-test.run=^TestRuntimeProcessHelper$", "--", labels[index], directory},
				"environment":   map[string]string{"LOCAL_RUNTIME_MCP_RUNTIME_PROCESS_HELPER": "1", "GORACE": "atexit_sleep_ms=0"},
				"yield_time_ms": 1, "timeout_seconds": 15,
			})
			launched <- launch{index, result, err}
		}(index, client)
	}
	close(barrier)
	results := make([]runtimeprocess.Result, 2)
	for range 2 {
		value := <-launched
		if value.err != nil || !value.result.Running || value.result.SessionID == "" {
			t.Fatalf("launch %s: %+v, %v", labels[value.index], value.result, value.err)
		}
		results[value.index] = value.result
	}
	if results[0].SessionID == results[1].SessionID {
		t.Fatal("independent process launches shared a session_id")
	}
	if first, second := recorderA.firstID("process_run"), recorderB.firstID("process_run"); first == "" || first != second {
		t.Fatalf("expected independently reused JSON-RPC IDs across entrances: %q/%q", first, second)
	}
	for _, label := range labels {
		waitForProcessHelperFile(t, ctx, filepath.Join(directory, label+".ready"), nil)
	}

	// The public client can read the private client's exact process identity.
	// Both helpers remain blocked, so this cannot depend on completion timing.
	shared, err := callProcessTool(ctx, b, "process_continue", map[string]any{
		"session_id": results[0].SessionID, "output_cursor": results[0].OutputCursor, "yield_time_ms": 1,
	})
	if err != nil || !shared.Running || shared.SessionID != results[0].SessionID {
		t.Fatalf("public entrance could not address private process: %+v, %v", shared, err)
	}

	// Closing one listener must not cancel the Host, the other listener, or
	// processes launched by that entrance. No shared parent context is canceled.
	stopPrivate()
	if err := ctx.Err(); err != nil {
		t.Fatalf("private closure canceled the Host context: %v", err)
	}
	stillRunning, err := callProcessTool(ctx, b, "process_continue", map[string]any{
		"session_id": results[0].SessionID, "output_cursor": shared.OutputCursor, "yield_time_ms": 1,
	})
	if err != nil || !stillRunning.Running || stillRunning.SessionID != results[0].SessionID {
		t.Fatalf("private process was lost with its entrance: %+v, %v", stillRunning, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, label := range labels {
		waitForProcessHelperFile(t, ctx, filepath.Join(directory, label+".done"), nil)
	}
	for index, result := range results {
		cursor := result.OutputCursor
		output := result.Stdout
		for {
			continued, err := callProcessTool(ctx, b, "process_continue", map[string]any{
				"session_id": result.SessionID, "output_cursor": cursor, "yield_time_ms": 1000,
			})
			if err != nil || continued.Stderr != "" || continued.TimedOut {
				t.Fatalf("public collection %s: %+v, %v", labels[index], continued, err)
			}
			output += continued.Stdout
			cursor = continued.OutputCursor
			if !continued.Running {
				if continued.ExitCode != 0 {
					t.Fatalf("helper %s exited %d", labels[index], continued.ExitCode)
				}
				break
			}
		}
		want := fmt.Sprintf("%s tick=1\n%s tick=2\n%s tick=3\n", labels[index], labels[index], labels[index])
		if strings.ReplaceAll(output, "\r\n", "\n") != want {
			t.Fatalf("response or output mixed between entrances: got=%q want=%q", output, want)
		}
		// Retained completed output is independently readable by a fresh caller
		// even after its original entrance has disappeared.
		replayed, err := callProcessTool(ctx, b, "process_continue", map[string]any{"session_id": result.SessionID})
		if err != nil || replayed.Running || replayed.ExitCode != 0 || strings.ReplaceAll(replayed.Stdout, "\r\n", "\n") != want {
			t.Fatalf("completed output retention %s: %+v, %v", labels[index], replayed, err)
		}
	}

	var info filesystem.FileInfo
	path := filepath.Join(directory, labels[0]+".ready")
	if err := multiIngressCall(ctx, b, "filesystem_stat", map[string]any{"path": path}, &info); err != nil || info.Path != path || info.Type != "file" {
		t.Fatalf("remaining entrance filesystem read: %+v, %v", info, err)
	}
	var status browser.Status
	if err := multiIngressCall(ctx, b, "browser_status", map[string]any{}, &status); err != nil || !status.Configured || status.Address == "" {
		t.Fatalf("shared bridge was lost with private entrance: %+v, %v", status, err)
	}
	var control computer.ControlStatus
	if err := multiIngressCall(ctx, b, "computer_control", map[string]any{"kind": "status"}, &control); err != nil || control.Status != "free" || control.ControlID != "" {
		t.Fatalf("read-only desktop coordination state: %+v, %v", control, err)
	}
	catalog, err := b.ListTools(ctx, nil)
	if err != nil || len(catalog.Tools) != 21 {
		t.Fatalf("remaining public entrance catalog: result=%+v err=%v", catalog, err)
	}
}

func multiIngressToken(t *testing.T) string {
	t.Helper()
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(data[:])
}

func multiIngressHTTP(t *testing.T, parent context.Context, host *Host, cfg transport.HTTPConfig) (string, func()) {
	t.Helper()
	origin, err := transport.NewHTTP(host.Server(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- origin.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("close entrance: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("entrance did not close")
			}
		})
	}
	t.Cleanup(stop)
	return "http://" + origin.Address() + "/mcp", stop
}

func multiIngressConnect(t *testing.T, ctx context.Context, endpoint string, roundTripper http.RoundTripper, name string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: roundTripper}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func multiIngressCall(ctx context.Context, client *mcp.ClientSession, name string, arguments, output any) error {
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return err
	}
	if result.IsError {
		return &toolFailure{result}
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, output)
}
