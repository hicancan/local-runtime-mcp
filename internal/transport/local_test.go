package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLocalHTTPRejectsBrowserProxyAndEnforcesTokenSeparation(t *testing.T) {
	ownerToken := strings.Repeat("o", 32)
	clientToken := strings.Repeat("c", 32)
	modelHits, capHits := 0, 0
	modelHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHits++
		if r.Header.Get("Authorization") != "Bearer "+clientToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	capHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capHits++
		if r.Header.Get("Authorization") != "Bearer "+clientToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	if _, err := NewLocalHTTP(mcpSrv, LocalConfig{Listen: "0.0.0.0:0", Token: ownerToken}); err == nil {
		t.Fatal("a non-loopback address was accepted")
	}
	server, err := NewLocalHTTP(mcpSrv, LocalConfig{
		Listen:       "127.0.0.1:0",
		Token:        ownerToken,
		Version:      "15.0.0",
		Models:       modelHandler,
		Capabilities: capHandler,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.listener.Close() })

	for _, test := range []struct {
		name, method, path, host, remote, bearer string
		header, value                            string
		want                                     int
	}{
		{"health ok", http.MethodGet, "/health", "127.0.0.1", "127.0.0.1:4567", ownerToken, "", "", http.StatusOK},
		{"health rejects client token", http.MethodGet, "/health", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusUnauthorized},
		{"mcp rejects client token", http.MethodPost, "/mcp", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusUnauthorized},
		{"mcp missing token", http.MethodPost, "/mcp", "localhost", "127.0.0.1:4567", "", "", "", http.StatusUnauthorized},
		{"models ok", http.MethodGet, "/v1/models", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusNoContent},
		{"models rejects owner token", http.MethodGet, "/v1/models", "127.0.0.1", "127.0.0.1:4567", ownerToken, "", "", http.StatusUnauthorized},
		{"capabilities ok", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusOK},
		{"capabilities rejects owner token", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", ownerToken, "", "", http.StatusUnauthorized},
		{"remote caller blocked", http.MethodGet, "/health", "127.0.0.1", "192.0.2.1:4567", ownerToken, "", "", http.StatusForbidden},
		{"public host blocked", http.MethodGet, "/health", "public.example", "127.0.0.1:4567", ownerToken, "", "", http.StatusForbidden},
		{"browser origin blocked", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", clientToken, "Origin", "https://studev.example", http.StatusForbidden},
		{"empty origin blocked", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", clientToken, "Origin", "", http.StatusForbidden},
		{"reverse proxy blocked", http.MethodGet, "/v1/models", "127.0.0.1", "127.0.0.1:4567", clientToken, "Forwarded", "for=192.0.2.1", http.StatusForbidden},
		{"cloudflare header blocked", http.MethodGet, "/mcp", "127.0.0.1", "127.0.0.1:4567", ownerToken, "CF-Connecting-IP", "192.0.2.1", http.StatusForbidden},
		{"unknown path", http.MethodGet, "/oauth/token", "127.0.0.1", "127.0.0.1:4567", ownerToken, "", "", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, test.path, nil)
			r.Host, r.RemoteAddr = test.host, test.remote
			if test.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+test.bearer)
			}
			if test.header != "" {
				r.Header.Set(test.header, test.value)
			}
			w := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(w, r)
			if w.Code != test.want || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), ownerToken) || strings.Contains(w.Body.String(), clientToken) {
				t.Fatalf("unexpected local response %d %s", w.Code, w.Body.String())
			}
		})
	}
	if modelHits != 2 || capHits != 2 {
		t.Fatalf("blocked request reached inner handlers: modelHits=%d capHits=%d", modelHits, capHits)
	}
}

func TestPublicHTTPNeverMountsModelsOrCapabilities(t *testing.T) {
	instance, err := NewHTTP(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil), HTTPConfig{
		Listen: "127.0.0.1:0", BearerToken: testBearerToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	endpoint := "http://" + instance.Address()
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/capabilities", "/capabilities/invoke", "/health"} {
		resp := request(t, endpoint+path, testBearerToken, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("public HTTP unexpectedly mounted %s: %d", path, resp.StatusCode)
		}
	}
}
