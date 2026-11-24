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
	adminHits, capHits := 0, 0
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminHits++
		w.WriteHeader(http.StatusOK)
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
		Version:      "16.0.0",
		Capabilities: capHandler,
		Admin:        adminHandler,
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
		{"admin ok", http.MethodGet, "/admin/status", "127.0.0.1", "127.0.0.1:4567", ownerToken, "", "", http.StatusOK},
		{"admin rejects client token", http.MethodGet, "/admin/status", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusUnauthorized},
		{"admin rejects browser origin", http.MethodGet, "/admin/status", "127.0.0.1", "127.0.0.1:4567", ownerToken, "Origin", "https://evil.example", http.StatusForbidden},
		{"mcp rejects client token", http.MethodPost, "/mcp", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusUnauthorized},
		{"mcp missing token", http.MethodPost, "/mcp", "localhost", "127.0.0.1:4567", "", "", "", http.StatusUnauthorized},
		{"models route removed", http.MethodGet, "/v1/models", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusNotFound},
		{"capabilities ok", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", clientToken, "", "", http.StatusOK},
		{"capabilities rejects owner token", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", ownerToken, "", "", http.StatusUnauthorized},
		{"remote caller blocked", http.MethodGet, "/health", "127.0.0.1", "192.0.2.1:4567", ownerToken, "", "", http.StatusForbidden},
		{"public host blocked", http.MethodGet, "/health", "public.example", "127.0.0.1:4567", ownerToken, "", "", http.StatusForbidden},
		{"browser origin blocked", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", clientToken, "Origin", "https://studev.example", http.StatusForbidden},
		{"empty origin blocked", http.MethodGet, "/capabilities", "127.0.0.1", "127.0.0.1:4567", clientToken, "Origin", "", http.StatusForbidden},
		{"reverse proxy blocked", http.MethodGet, "/admin/status", "127.0.0.1", "127.0.0.1:4567", ownerToken, "Forwarded", "for=192.0.2.1", http.StatusForbidden},
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
	if adminHits != 1 || capHits != 2 {
		t.Fatalf("blocked request reached inner handlers: adminHits=%d capHits=%d", adminHits, capHits)
	}
}

func TestPublicHTTPNeverMountsAdminOrCapabilities(t *testing.T) {
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
	for _, path := range []string{"/admin/status", "/v1/models", "/capabilities", "/capabilities/invoke", "/health"} {
		resp := request(t, endpoint+path, testBearerToken, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("public HTTP unexpectedly mounted %s: %d", path, resp.StatusCode)
		}
	}
}
