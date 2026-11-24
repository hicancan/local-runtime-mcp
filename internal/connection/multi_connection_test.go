package connection

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
)

func TestExplicitTransportAndProviderSelection(t *testing.T) {
	for _, value := range []Options{
		{Transport: "stdio"},
		{Transport: "daemon"},
		{Transport: "daemon", HTTP: true},
		{Transport: "daemon", Tunnels: []string{"openai"}},
		{Transport: "daemon", Tunnels: []string{"cloudflare"}},
		{Transport: "daemon", HTTP: true, Tunnels: []string{"openai", "cloudflare"}},
	} {
		if err := value.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []Options{
		{},
		{Transport: "sse"},
		{Transport: "local"},
		{Transport: "http"},
		{Transport: "stdio", HTTP: true},
		{Transport: "stdio", Tunnels: []string{"openai"}},
		{Transport: "daemon", Tunnels: []string{"unknown"}},
		{Transport: "daemon", Tunnels: []string{"openai", "openai"}},
	} {
		if value.Validate() == nil {
			t.Fatalf("invalid selection accepted: %+v", value)
		}
	}
}

func TestIngressManagerHotPlugTTLAndIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	httpAddr := reservePort(t)
	cfg := config.Default()
	cfg.Browser = config.Browser{Listen: "127.0.0.1:0", Token: strings.Repeat("b", 32)}
	cfg.Local = config.Local{Listen: "127.0.0.1:0", Token: strings.Repeat("l", 32)}
	cfg.HTTP = config.HTTP{Listen: httpAddr, PublicHost: "mcp.example.com", Auth: "bearer", BearerToken: strings.Repeat("p", 32)}
	cfg.OpenAI = config.OpenAI{TunnelID: "tunnel_0123456789abcdef0123456789abcdef", APIKey: "sk-proj-test-key-0123456789abcdef"}

	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	var log bytes.Buffer
	mgr := newIngressManager(ctx, cfg, host, &log, func() int { return 2 })
	t.Cleanup(mgr.Close)

	var openaiStarts atomic.Int32
	openaiStarted := make(chan struct{}, 4)
	openaiFail := make(chan error, 1)
	mgr.runOpenAI = func(wctx context.Context, _ *config.Config, _ *runtimehost.Host) error {
		openaiStarts.Add(1)
		openaiStarted <- struct{}{}
		select {
		case err := <-openaiFail:
			return err
		case <-wctx.Done():
			return wctx.Err()
		}
	}

	// Initially all wires are stopped.
	for _, w := range mgr.Status() {
		if w.State != "stopped" {
			t.Fatalf("expected stopped initially: %+v", w)
		}
	}

	// Hot-plug public HTTP wire.
	statuses, err := mgr.Up("http", 0)
	if err != nil {
		t.Fatal(err)
	}
	if findWire(statuses, "http").State != "active" {
		t.Fatalf("http wire not active: %+v", statuses)
	}

	// Hot-plug OpenAI wire with a short TTL lease; verify it auto-detaches without disturbing HTTP.
	if _, err := mgr.Up("openai", 250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-openaiStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("openai runner did not start")
	}
	if findWire(mgr.Status(), "openai").State != "active" || openaiStarts.Load() != 1 {
		t.Fatalf("openai wire did not start: %+v", mgr.Status())
	}
	deadline := time.Now().Add(3 * time.Second)
	for findWire(mgr.Status(), "openai").State != "stopped" {
		if time.Now().After(deadline) {
			t.Fatalf("openai lease did not expire: %+v", mgr.Status())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if findWire(mgr.Status(), "http").State != "active" {
		t.Fatalf("http wire was disturbed by openai lease expiry: %+v", mgr.Status())
	}

	// Re-attach OpenAI and inject a provider error; HTTP must survive and OpenAI reports error.
	if _, err := mgr.Up("openai", 0); err != nil {
		t.Fatal(err)
	}
	openaiFail <- errors.New("upstream disconnect")
	deadline = time.Now().Add(3 * time.Second)
	for findWire(mgr.Status(), "openai").State != "error" {
		if time.Now().After(deadline) {
			t.Fatalf("openai error state not observed: %+v", mgr.Status())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !strings.Contains(findWire(mgr.Status(), "openai").LastError, "upstream disconnect") {
		t.Fatalf("unexpected openai error: %+v", findWire(mgr.Status(), "openai"))
	}
	if findWire(mgr.Status(), "http").State != "active" {
		t.Fatalf("http wire died when openai failed: %+v", mgr.Status())
	}

	// Hot-unplug all wires cleanly.
	if _, err := mgr.Down("all"); err != nil {
		t.Fatal(err)
	}
	for _, w := range mgr.Status() {
		if w.State != "stopped" {
			t.Fatalf("wire did not stop: %+v", w)
		}
	}
}

func TestPublicAuthenticationIsSelectedIndependently(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.PublicHost = "mcp.example.com"
	cfg.HTTP.BearerToken = "static-credential-not-an-oauth-token"
	got, err := publicHTTPConfig(cfg)
	if err != nil || got.OAuth != nil || got.InternalToken {
		t.Fatal("static HTTP mode changed")
	}
	cfg.HTTP.Auth = "oauth"
	got, err = publicHTTPConfig(cfg)
	if err != nil || got.OAuth == nil || got.OAuth.Issuer != "https://mcp.example.com" || got.InternalToken {
		t.Fatalf("OAuth entrance: %v", err)
	}
	cfg.HTTP.Auth = "invalid"
	if _, err = publicHTTPConfig(cfg); err == nil {
		t.Fatal("invalid auth accepted")
	}
}

func findWire(list []runtimehost.TunnelStatus, name string) runtimehost.TunnelStatus {
	for _, item := range list {
		if item.Name == name {
			return item
		}
	}
	return runtimehost.TunnelStatus{}
}

var _ = io.Discard
