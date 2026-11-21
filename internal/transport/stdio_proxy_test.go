package transport

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioProxyAttachesMultipleClientsAndForwardsToolUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ownerToken := strings.Repeat("p", 32)
	server := mcp.NewServer(&mcp.Implementation{Name: "local-runtime-mcp", Version: "15.0.0"}, nil)
	type pingIn struct {
		Message string `json:"message"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "Echo message"}, func(_ context.Context, _ *mcp.CallToolRequest, in pingIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong:" + in.Message}}}, nil, nil
	})

	localHTTP, err := NewLocalHTTP(server, LocalConfig{
		Listen:  "127.0.0.1:0",
		Token:   ownerToken,
		Version: "15.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpDone := make(chan error, 1)
	go func() { httpDone <- localHTTP.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-httpDone; err != nil {
			t.Errorf("local HTTP shutdown: %v", err)
		}
	})

	if ProbeLocal(ctx, localHTTP.Address(), strings.Repeat("x", 32)) {
		t.Fatal("ProbeLocal accepted wrong token")
	}
	if !ProbeLocal(ctx, localHTTP.Address(), ownerToken) {
		t.Fatal("ProbeLocal failed against live local daemon")
	}

	var changedCount atomic.Int32
	notifyCh := make(chan struct{}, 4)

	connectProxy := func(name string) (*mcp.ClientSession, chan error) {
		t.Helper()
		clientToProxyR, clientToProxyW := io.Pipe()
		proxyToClientR, proxyToClientW := io.Pipe()
		proxyDone := make(chan error, 1)
		go func() {
			err := ProxyStdioToLocal(ctx, localHTTP.Address(), ownerToken, clientToProxyR, proxyToClientW)
			_ = proxyToClientW.Close()
			proxyDone <- err
		}()
		client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, &mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				changedCount.Add(1)
				notifyCh <- struct{}{}
			},
		})
		session, err := client.Connect(ctx, &mcp.IOTransport{
			Reader: proxyToClientR,
			Writer: clientToProxyW,
		}, nil)
		if err != nil {
			_ = clientToProxyW.Close()
			t.Fatalf("connect %s through stdio proxy: %v (proxy=%v)", name, err, <-proxyDone)
		}
		t.Cleanup(func() {
			_ = session.Close()
			_ = clientToProxyW.Close()
			_ = proxyToClientR.Close()
			select {
			case err := <-proxyDone:
				if err != nil {
					t.Errorf("proxy %s returned error: %v", name, err)
				}
			case <-time.After(3 * time.Second):
				t.Errorf("proxy %s did not exit after client closed", name)
			}
		})
		return session, proxyDone
	}

	s1, _ := connectProxy("codex-1")
	s2, _ := connectProxy("codex-2")

	for idx, s := range []*mcp.ClientSession{s1, s2} {
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{"message": string(rune('a' + idx))}})
		if err != nil || res.IsError || len(res.Content) == 0 {
			t.Fatalf("client %d ping failed: %v %+v", idx, err, res)
		}
	}

	// Dynamically add a tool on the shared daemon; both attached stdio proxies must receive tools/list_changed.
	mcp.AddTool(server, &mcp.Tool{Name: "chaoxing.chapters", Description: "Approved tool"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})

	for range 2 {
		select {
		case <-notifyCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("expected tools/list_changed on both stdio proxies, got %d", changedCount.Load())
		}
	}
	list, err := s2.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 2 {
		t.Fatalf("proxy did not observe newly approved tool: %v %+v", err, list)
	}
}
