package connection

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func reservePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestUnifiedDaemonSharesOneHostAcrossMultipleStdioProxiesAndPublicHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	localAddr := reservePort(t)
	publicAddr := reservePort(t)
	browserAddr := reservePort(t)

	localToken := strings.Repeat("l", 32)
	publicToken := strings.Repeat("p", 32)
	browserToken := strings.Repeat("b", 32)
	clientToken := strings.Repeat("c", 32)

	cfg := &config.Config{
		Browser: config.Browser{Listen: browserAddr, Token: browserToken},
		Local:   config.Local{Listen: localAddr, Token: localToken},
		HTTP:    config.HTTP{Listen: publicAddr, Auth: "bearer", BearerToken: publicToken},
		Grants: config.Grants{
			Clients: []config.ClientGrant{
				{ID: "studev-win", Token: clientToken},
			},
		},
	}

	daemonCtx, stopDaemon := context.WithCancel(ctx)
	var daemonLog bytes.Buffer
	daemonDone := make(chan error, 1)
	go func() {
		daemonDone <- Run(daemonCtx, cfg, Options{Transport: "http"}, strings.NewReader(""), io.Discard, &daemonLog)
	}()
	t.Cleanup(func() {
		stopDaemon()
		if err := <-daemonDone; err != nil {
			t.Errorf("daemon shutdown: %v (log=%s)", err, daemonLog.String())
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for !transport.ProbeLocal(ctx, localAddr, localToken) {
		if time.Now().After(deadline) {
			t.Fatalf("daemon local plane never became ready; log=%s", daemonLog.String())
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Launch two concurrent stdio clients; both must attach as thin proxies to the
	// running daemon rather than starting competing Hosts.
	connectStdio := func(name string) (*mcp.ClientSession, *bytes.Buffer) {
		t.Helper()
		cToPR, cToPW := io.Pipe()
		pToCR, pToCW := io.Pipe()
		var stderr bytes.Buffer
		done := make(chan error, 1)
		go func() {
			err := Run(ctx, cfg, Options{Transport: "stdio"}, cToPR, pToCW, &stderr)
			_ = pToCW.Close()
			done <- err
		}()
		client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.IOTransport{Reader: pToCR, Writer: cToPW}, nil)
		if err != nil {
			t.Fatalf("connect %s: %v (stderr=%s)", name, err, stderr.String())
		}
		t.Cleanup(func() {
			_ = session.Close()
			_ = cToPW.Close()
			_ = pToCR.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("stdio proxy %s error: %v", name, err)
				}
			case <-time.After(3 * time.Second):
				t.Errorf("stdio proxy %s stuck on exit", name)
			}
		})
		return session, &stderr
	}

	codex1, log1 := connectStdio("codex-1")
	codex2, log2 := connectStdio("codex-2")

	if !strings.Contains(log1.String(), "attached to running local runtime") || !strings.Contains(log2.String(), "attached to running local runtime") {
		t.Fatalf("expected stdio thin proxy attach; log1=%q log2=%q", log1.String(), log2.String())
	}

	// Both proxies see the SAME configured browser bridge (zero 9315 conflict!).
	for idx, session := range []*mcp.ClientSession{codex1, codex2} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "browser_status", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("codex-%d browser_status failed: %v %+v", idx+1, err, res)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if !strings.Contains(string(raw), browserAddr) || strings.Contains(string(raw), "not listening") {
			t.Fatalf("codex-%d lost shared browser bridge: %s", idx+1, raw)
		}
	}

	// Verify scoped local client token works on 9317 /v1/models and /capabilities,
	// while being rejected on 9317 /mcp and 9316 /mcp.
	directHTTP := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	check := func(method, url, token string, want int) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := directHTTP.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d", method, url, resp.StatusCode, want)
		}
	}
	check(http.MethodGet, "http://"+localAddr+"/v1/models", clientToken, http.StatusOK)
	check(http.MethodGet, "http://"+localAddr+"/capabilities", clientToken, http.StatusOK)
	check(http.MethodPost, "http://"+localAddr+"/mcp", clientToken, http.StatusUnauthorized)
	check(http.MethodGet, "http://"+localAddr+"/v1/models", localToken, http.StatusUnauthorized)
	check(http.MethodGet, "http://"+publicAddr+"/v1/models", clientToken, http.StatusNotFound)
	check(http.MethodGet, "http://"+publicAddr+"/capabilities", clientToken, http.StatusNotFound)
}

func TestFirstStdioHostsLocalPlaneSoSecondStdioAttaches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	localAddr := reservePort(t)
	browserAddr := reservePort(t)
	cfg := &config.Config{
		Browser: config.Browser{Listen: browserAddr, Token: strings.Repeat("b", 32)},
		Local:   config.Local{Listen: localAddr, Token: strings.Repeat("l", 32)},
	}

	connect := func(name string) (*mcp.ClientSession, *bytes.Buffer) {
		t.Helper()
		cToPR, cToPW := io.Pipe()
		pToCR, pToCW := io.Pipe()
		var stderr bytes.Buffer
		done := make(chan error, 1)
		go func() {
			err := Run(ctx, cfg, Options{Transport: "stdio"}, cToPR, pToCW, &stderr)
			_ = pToCW.Close()
			done <- err
		}()
		client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.IOTransport{Reader: pToCR, Writer: cToPW}, nil)
		if err != nil {
			t.Fatalf("connect %s: %v (stderr=%s)", name, err, stderr.String())
		}
		t.Cleanup(func() {
			_ = session.Close()
			_ = cToPW.Close()
			_ = pToCR.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s error: %v", name, err)
				}
			case <-time.After(3 * time.Second):
				t.Errorf("%s did not exit cleanly", name)
			}
		})
		return session, &stderr
	}

	// First stdio starts the Host + local plane.
	first, log1 := connect("codex-first")
	if strings.Contains(log1.String(), "attached to running local runtime") {
		t.Fatalf("first stdio unexpectedly attached: %s", log1.String())
	}
	// Second stdio discovers the local plane hosted by the first and attaches cleanly.
	second, log2 := connect("codex-second")
	if !strings.Contains(log2.String(), "attached to running local runtime") {
		t.Fatalf("second stdio did not attach to first: %s", log2.String())
	}
	for idx, s := range []*mcp.ClientSession{first, second} {
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "browser_status", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("session %d browser_status failed: %v %+v", idx+1, err, res)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if !strings.Contains(string(raw), browserAddr) || strings.Contains(string(raw), "not listening") {
			t.Fatalf("session %d did not share browser bridge: %s", idx+1, raw)
		}
	}
}
