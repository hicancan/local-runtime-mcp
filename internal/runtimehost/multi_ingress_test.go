package runtimehost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/hicancan/local-runtime-mcp/internal/jsrt"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
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
	clone.Header.Set("Authorization", "Bearer forwarded-client-credential")
	clone.Header.Set("Origin", "https://chatgpt.com")
	return h.base.RoundTrip(clone)
}

// Exercise two actual HTTP listeners sharing one Host, proving that closing one
// listener does not affect the Host, running cells, or the other listener.
func TestMultiIngressHostSharesSessionsAndSurvivesPrivateClosure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	bridgeToken := multiIngressToken(t)
	host, err := Start(ctx, &config.Config{
		Browser: config.Browser{Listen: "127.0.0.1:0", Token: bridgeToken, DisableAutoCDP: true},
	})
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

	a := multiIngressConnect(t, ctx, privateOrigin, multiIngressPrivateHop{http.DefaultTransport, privateToken}, "private-client")
	b := multiIngressConnect(t, ctx, publicOrigin, authorization{http.DefaultTransport, publicToken}, "public-client")

	for _, session := range []*mcp.ClientSession{a, b} {
		identity := session.InitializeResult().ServerInfo
		if identity.Name != "local-runtime-mcp" || identity.Version != mcpserver.Version {
			t.Fatalf("unexpected shared Server identity: %+v", identity)
		}
		catalog, err := session.ListTools(ctx, nil)
		if err != nil || len(catalog.Tools) != 3 {
			t.Fatalf("expected 3 remote core tools, got %+v err=%v", catalog, err)
		}
	}

	// Start a long-running cell through the private entrance, then close the private entrance
	// and collect the cell's completion through the public entrance!
	runRes, err := a.CallTool(ctx, &mcp.CallToolParams{
		Name: "run",
		Arguments: map[string]any{
			"title":    "Async cell across entrances",
			"yield_ms": 40,
			"code":     `await sleep(180); print("survived-closure"); return "shared-ok";`,
		},
	})
	if err != nil || runRes.IsError {
		t.Fatalf("launch private cell failed: %v %+v", err, runRes)
	}
	var c1 jsrt.CellResult
	if err := decodeToolJSON(runRes, &c1); err != nil {
		t.Fatalf("decode run result: %v", err)
	}
	if c1.Status != "running" || c1.CellID == "" {
		t.Fatalf("expected running cell_id, got %+v", c1)
	}

	stopPrivate()
	if err := ctx.Err(); err != nil {
		t.Fatalf("private closure canceled Host context: %v", err)
	}

	waitRes, err := b.CallTool(ctx, &mcp.CallToolParams{
		Name:      "wait",
		Arguments: map[string]any{"cell_id": c1.CellID, "yield_ms": 3000},
	})
	if err != nil || waitRes.IsError {
		t.Fatalf("public entrance could not collect private cell: %v %+v", err, waitRes)
	}
	var c2 jsrt.CellResult
	if err := decodeToolJSON(waitRes, &c2); err != nil {
		t.Fatalf("decode wait result: %v", err)
	}
	if c2.Status != "done" || c2.Value != "shared-ok" || !strings.Contains(c2.Output, "survived-closure") {
		t.Fatalf("unexpected collected cell: %+v", c2)
	}

	// Verify fs, browser, and desktop SDKs remain healthy on remaining public entrance.
	tmpFile := filepath.Join(t.TempDir(), "probe.txt")
	_ = os.WriteFile(tmpFile, []byte("ok"), 0o600)
	probeRes, err := b.CallTool(ctx, &mcp.CallToolParams{
		Name: "run",
		Arguments: map[string]any{
			"title": "Verify shared subsystems",
			"code": `
				const [st, br, ctl] = await Promise.all([
					fs.stat("` + filepath.ToSlash(tmpFile) + `"),
					browser.status(),
					desktop.control("status"),
				]);
				return { file: st, browser: br, control: ctl };
			`,
		},
	})
	if err != nil || probeRes.IsError {
		t.Fatalf("remaining entrance probe failed: %v %+v", err, probeRes)
	}
	var probe struct {
		Value struct {
			File    filesystem.FileInfo    `json:"file"`
			Browser browser.Status         `json:"browser"`
			Control computer.ControlStatus `json:"control"`
		} `json:"value"`
	}
	if err := decodeToolJSON(probeRes, &probe); err != nil {
		t.Fatalf("decode probe result: %v", err)
	}
	if probe.Value.File.Type != "file" || !probe.Value.Browser.Configured || probe.Value.Control.Status != "free" {
		t.Fatalf("unexpected probe result: %+v", probe.Value)
	}
}

func multiIngressToken(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func multiIngressHTTP(t *testing.T, ctx context.Context, host *Host, cfg transport.HTTPConfig) (string, func()) {
	t.Helper()
	srv := host.Server()
	if cfg.InternalToken {
		srv = host.RemoteServer("openai")
	}
	httpServer, err := transport.NewHTTP(srv, cfg)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- httpServer.Run(runCtx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("http server shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("http server did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return "http://" + httpServer.Address() + "/mcp", stop
}

func multiIngressConnect(t *testing.T, ctx context.Context, endpoint string, rt http.RoundTripper, name string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: rt},
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
