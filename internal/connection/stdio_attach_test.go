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

func TestUnifiedDaemonSharesOneHostAcrossStdioProxiesAndHotPlugsHTTP(t *testing.T) {
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
		daemonDone <- Run(daemonCtx, cfg, Options{Transport: "daemon"}, strings.NewReader(""), io.Discard, &daemonLog)
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

	client1, log1 := connectStdio("client-1")
	client2, log2 := connectStdio("client-2")

	if !strings.Contains(log1.String(), "attached to running local runtime") || !strings.Contains(log2.String(), "attached to running local runtime") {
		t.Fatalf("expected stdio thin proxy attach; log1=%q log2=%q", log1.String(), log2.String())
	}

	// Admin status reports 2 attached stdio sessions and stopped remote wires.
	var status DaemonStatus
	if err := transport.AdminRequest(ctx, localAddr, localToken, http.MethodGet, "/admin/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status.AttachedStdio != 2 {
		t.Fatalf("expected 2 attached stdio clients, got %d", status.AttachedStdio)
	}
	if findWire(status.Tunnels, "http").State != "stopped" {
		t.Fatalf("expected http wire stopped before hot-plug: %+v", status.Tunnels)
	}

	// Hot-plug public HTTP wire via /admin/tunnels/up without restarting daemon or dropping stdio proxies.
	if err := transport.AdminRequest(ctx, localAddr, localToken, http.MethodPost, "/admin/tunnels/up", map[string]any{"name": "http"}, nil); err != nil {
		t.Fatalf("hot-plug http wire failed: %v", err)
	}

	// Both proxies still see the SAME configured browser bridge via `run`.
	for idx, session := range []*mcp.ClientSession{client1, client2} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "run", Arguments: map[string]any{
			"title": "Check shared browser bridge",
			"code":  "return await browser.status();",
		}})
		if err != nil || res.IsError {
			t.Fatalf("client-%d browser.status failed: %v %+v", idx+1, err, res)
		}
		raw, _ := json.Marshal(res.Content)
		if !strings.Contains(string(raw), browserAddr) || strings.Contains(string(raw), "unavailable") {
			t.Fatalf("client-%d lost shared browser bridge: %s", idx+1, raw)
		}
	}

	// Verify token boundaries on 9317 and hot-plugged 9316.
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
	check(http.MethodGet, "http://"+localAddr+"/capabilities", clientToken, http.StatusOK)
	check(http.MethodPost, "http://"+localAddr+"/mcp", clientToken, http.StatusUnauthorized)
	check(http.MethodGet, "http://"+localAddr+"/admin/status", clientToken, http.StatusUnauthorized)
	check(http.MethodGet, "http://"+localAddr+"/v1/models", clientToken, http.StatusNotFound)
	check(http.MethodGet, "http://"+publicAddr+"/capabilities", clientToken, http.StatusNotFound)
	check(http.MethodGet, "http://"+publicAddr+"/admin/status", localToken, http.StatusNotFound)

	// Hot-unplug public HTTP wire cleanly while stdio clients stay alive.
	if err := transport.AdminRequest(ctx, localAddr, localToken, http.MethodPost, "/admin/tunnels/down", map[string]any{"name": "http"}, nil); err != nil {
		t.Fatalf("hot-unplug http wire failed: %v", err)
	}
	if err := transport.AdminRequest(ctx, localAddr, localToken, http.MethodGet, "/admin/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	if findWire(status.Tunnels, "http").State != "stopped" || status.AttachedStdio != 2 {
		t.Fatalf("unexpected status after hot-unplug: %+v", status)
	}
}

func TestFirstStdioHostsLocalPlaneInProcessWhenAutoSpawnDisabled(t *testing.T) {
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
			err := Run(ctx, cfg, Options{Transport: "stdio", AutoSpawnDaemon: false}, cToPR, pToCW, &stderr)
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

	first, log1 := connect("first")
	if strings.Contains(log1.String(), "attached to running local runtime") {
		t.Fatalf("first stdio unexpectedly attached: %s", log1.String())
	}
	second, log2 := connect("second")
	if !strings.Contains(log2.String(), "attached to running local runtime") {
		t.Fatalf("second stdio did not attach to first: %s", log2.String())
	}
	for idx, s := range []*mcp.ClientSession{first, second} {
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "run", Arguments: map[string]any{
			"title": "Check shared browser bridge",
			"code":  "return await browser.status();",
		}})
		if err != nil || res.IsError {
			t.Fatalf("session %d browser.status failed: %v %+v", idx+1, err, res)
		}
		raw, _ := json.Marshal(res.Content)
		if !strings.Contains(string(raw), browserAddr) || strings.Contains(string(raw), "unavailable") {
			t.Fatalf("session %d did not share browser bridge: %s", idx+1, raw)
		}
	}
}
