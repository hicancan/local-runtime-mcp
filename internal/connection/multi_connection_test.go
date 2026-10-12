package connection

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func TestExplicitTransportAndProviderSelection(t *testing.T) {
	for _, value := range []Options{{Transport: "stdio"}, {Transport: "http"}, {Transport: "http", Tunnels: []string{"openai"}}, {Transport: "http", Tunnels: []string{"cloudflare"}}, {Transport: "http", Tunnels: []string{"openai", "cloudflare"}}} {
		if err := value.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []Options{{}, {Transport: "sse"}, {Transport: "stdio", Tunnels: []string{"openai"}}, {Transport: "http", Tunnels: []string{"unknown"}}, {Transport: "http", Tunnels: []string{"openai", "openai"}}} {
		if value.Validate() == nil {
			t.Fatalf("invalid selection accepted: %+v", value)
		}
	}
}

func TestProviderFailureDoesNotCancelOtherEntrance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := errors.New("owned provider failed")
	peerReady := make(chan struct{})
	peerStopped := make(chan struct{})
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runComponents(ctx, []component{
			{"failed", func(context.Context) error { <-peerReady; return failed }},
			{"peer", func(ctx context.Context) error { close(peerReady); <-ctx.Done(); close(peerStopped); return ctx.Err() }},
		}, &lockedWriter{writer: &output})
	}()
	<-peerReady
	select {
	case <-peerStopped:
		t.Fatal("peer was cancelled by a different provider")
	case <-done:
		t.Fatal("root exited while another entrance was serving")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, failed) {
			t.Fatalf("lost provider error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("root shutdown stuck")
	}
	if !strings.Contains(output.String(), "[failed]") {
		t.Fatal("provider failure not labeled")
	}
}

func TestAllEndedEntrancesReturnWithoutCancellingHostContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runComponents(ctx, []component{{"first", func(context.Context) error { return nil }}, {"second", func(context.Context) error { return nil }}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("entrance altered its owner's context")
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
