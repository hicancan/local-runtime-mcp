package runtimehost

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type authorization struct {
	base  http.RoundTripper
	token string
}

func (a authorization) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(clone)
}

// Two independently connected clients operate one Host. Their first calls
// naturally reuse JSON-RPC sequence numbers; resource identity is independent.
func TestConcurrentClientsShareHostButNotResponseIdentity(t *testing.T) {
	ctx, a, b := sharedHTTPTestClients(t, http.DefaultTransport, http.DefaultTransport)
	directory := t.TempDir()
	errors := make(chan error, 2)
	for index, session := range []*mcp.ClientSession{a, b} {
		go func(index int, session *mcp.ClientSession) {
			name := []string{"A.txt", "B.txt"}[index]
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "filesystem_write_text", Arguments: map[string]any{
				"path": filepath.Join(directory, name), "content": name, "mode": "create",
			}})
			if err == nil && result.IsError {
				err = &toolFailure{result}
			}
			if err == nil {
				var written filesystem.TextWriteResult
				data, _ := json.Marshal(result.StructuredContent)
				err = json.Unmarshal(data, &written)
				if err == nil && written.Path != filepath.Join(directory, name) {
					err = &toolFailure{result}
				}
			}
			errors <- err
		}(index, session)
	}
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	_ = a.Close()
	tools, err := b.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 21 {
		t.Fatalf("remaining client: count=%v err=%v", tools, err)
	}
}

func sharedHTTPTestClients(t *testing.T, transportA, transportB http.RoundTripper) (context.Context, *mcp.ClientSession, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	host, err := Start(ctx, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := host.Close(shutdown); err != nil {
			t.Error(err)
		}
	})
	const token = "0123456789abcdef0123456789abcdef"
	httpServer, err := transport.NewHTTP(host.Server(), transport.HTTPConfig{Listen: "127.0.0.1:0", BearerToken: token})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	connect := func(base http.RoundTripper) *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "independent-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: "http://" + httpServer.Address() + "/mcp", DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: authorization{base, token}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	return ctx, connect(transportA), connect(transportB)
}

type toolFailure struct{ result *mcp.CallToolResult }

func (e *toolFailure) Error() string { data, _ := json.Marshal(e.result); return string(data) }
