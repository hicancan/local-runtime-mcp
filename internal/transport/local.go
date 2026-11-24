package transport

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultLocalListen = "127.0.0.1:9317"
	localUpgradeProto  = "lrmcp-stdio"
)

// LocalConfig configures the local-only loopback plane. This listener is never
// exposed to public tunnels or browsers; it serves same-computer MCP clients
// (including stdio thin proxies), the authenticated local admin control plane,
// and scoped local capability APIs.
type LocalConfig struct {
	Listen       string
	Token        string
	Version      string
	Capabilities http.Handler
	Admin        http.Handler
}

// LocalServer wraps the local-only loopback listener and exposes live attached
// stdio client counts.
type LocalServer struct {
	*HTTPServer
	attached *atomic.Int64
}

// AttachedClients reports how many stdio thin proxies are currently attached.
func (s *LocalServer) AttachedClients() int {
	if s == nil || s.attached == nil {
		return 0
	}
	return int(s.attached.Load())
}

// NewLocalHTTP creates the dedicated local-only listener.
// Every route rejects non-loopback peers, browser Origin headers, non-loopback
// Host headers, and reverse-proxy forwarding headers.
func NewLocalHTTP(localServer *mcp.Server, cfg LocalConfig) (*LocalServer, error) {
	if cfg.Listen == "" {
		cfg.Listen = DefaultLocalListen
	}
	if err := validateLoopbackAddress(cfg.Listen); err != nil {
		return nil, err
	}
	hasOwner := cfg.Token != ""
	if hasOwner && (len(cfg.Token) < 32 || strings.ContainsAny(cfg.Token, " \t\r\n")) {
		return nil, errors.New("local token must contain at least 32 characters without whitespace")
	}
	if !hasOwner && cfg.Capabilities == nil {
		return nil, errors.New("local listener requires a local token or at least one local client handler")
	}
	if hasOwner && localServer == nil {
		return nil, errors.New("local MCP server is required when local token is configured")
	}

	var mcpHandler http.Handler
	if hasOwner {
		mcpHandler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return localServer }, &mcp.StreamableHTTPOptions{
			Stateless:                    false,
			JSONResponse:                 true,
			DisableLocalhostProtection:   true,
			PropagateRequestCancellation: true,
			MaxRequestBodyBytes:          32 << 20,
		})
	}

	// Track active hijacked attach connections so server shutdown closes them cleanly.
	var attachMu sync.Mutex
	attachConns := map[net.Conn]struct{}{}
	attachCtx, cancelAttach := context.WithCancel(context.Background())
	var attached atomic.Int64

	server, err := newLoopbackHTTP(cfg.Listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		remoteHost, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(remoteHost)
		_, hasOrigin := r.Header["Origin"]
		if ip == nil || !ip.IsLoopback() || !allowedHost(r.Host, "") || hasOrigin || forwardedLocalRequest(r.Header) {
			writeLocalError(w, http.StatusForbidden, "local_only", "This endpoint is only available to local processes.")
			return
		}

		switch {
		case r.URL.Path == "/health":
			if !hasOwner {
				writeLocalError(w, http.StatusNotFound, "not_found", "Local MCP is not enabled.")
				return
			}
			if r.Method != http.MethodGet {
				writeLocalError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET for health checks.")
				return
			}
			if !validLocalOwnerToken(r, cfg.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-local"`)
				writeLocalError(w, http.StatusUnauthorized, "unauthorized", "A valid local token is required.")
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": cfg.Version})
		case r.URL.Path == "/mcp":
			if !hasOwner {
				writeLocalError(w, http.StatusNotFound, "not_found", "Local MCP is not enabled.")
				return
			}
			if !validLocalOwnerToken(r, cfg.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-local"`)
				writeLocalError(w, http.StatusUnauthorized, "unauthorized", "A valid local token is required.")
				return
			}
			mcpHandler.ServeHTTP(w, r)
		case r.URL.Path == "/mcp/attach":
			if !hasOwner {
				writeLocalError(w, http.StatusNotFound, "not_found", "Local MCP is not enabled.")
				return
			}
			if r.Method != http.MethodPost {
				writeLocalError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST to attach a stdio proxy.")
				return
			}
			if !validLocalOwnerToken(r, cfg.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-local"`)
				writeLocalError(w, http.StatusUnauthorized, "unauthorized", "A valid local token is required.")
				return
			}
			if !strings.EqualFold(r.Header.Get("Upgrade"), localUpgradeProto) {
				writeLocalError(w, http.StatusUpgradeRequired, "upgrade_required", "Upgrade to "+localUpgradeProto+" is required.")
				return
			}
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				writeLocalError(w, http.StatusInternalServerError, "hijack_failed", "Could not upgrade local stdio attach connection.")
				return
			}
			if _, err := io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+localUpgradeProto+"\r\n\r\n"); err != nil {
				_ = conn.Close()
				return
			}
			if err := rw.Flush(); err != nil {
				_ = conn.Close()
				return
			}
			attachMu.Lock()
			if attachCtx.Err() != nil {
				attachMu.Unlock()
				_ = conn.Close()
				return
			}
			attachConns[conn] = struct{}{}
			attachMu.Unlock()
			attached.Add(1)
			defer func() {
				attached.Add(-1)
				attachMu.Lock()
				delete(attachConns, conn)
				attachMu.Unlock()
				_ = conn.Close()
			}()
			_ = localServer.Run(attachCtx, &mcp.IOTransport{
				Reader: bufferedConnReader{Reader: rw.Reader, Closer: conn},
				Writer: conn,
			})
		case strings.HasPrefix(r.URL.Path, "/admin/"):
			if !hasOwner || cfg.Admin == nil {
				writeLocalError(w, http.StatusNotFound, "not_found", "Local admin control plane is not enabled.")
				return
			}
			if !validLocalOwnerToken(r, cfg.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-local"`)
				writeLocalError(w, http.StatusUnauthorized, "unauthorized", "A valid local token is required.")
				return
			}
			cfg.Admin.ServeHTTP(w, r)
		case r.URL.Path == "/capabilities" || r.URL.Path == "/capabilities/invoke":
			if cfg.Capabilities == nil {
				writeLocalError(w, http.StatusNotFound, "not_found", "Local capability API is not enabled.")
				return
			}
			cfg.Capabilities.ServeHTTP(w, r)
		default:
			writeLocalError(w, http.StatusNotFound, "not_found", "Unknown local runtime endpoint.")
		}
	}))
	if err != nil {
		cancelAttach()
		return nil, err
	}
	server.server.RegisterOnShutdown(func() {
		cancelAttach()
		attachMu.Lock()
		for conn := range attachConns {
			_ = conn.Close()
		}
		attachMu.Unlock()
	})
	return &LocalServer{HTTPServer: server, attached: &attached}, nil
}

type bufferedConnReader struct {
	*bufio.Reader
	io.Closer
}

func validLocalOwnerToken(r *http.Request, expected string) bool {
	provided, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return bearer && len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func forwardedLocalRequest(headers http.Header) bool {
	return headers.Get("Forwarded") != "" ||
		headers.Get("X-Forwarded-For") != "" ||
		headers.Get("X-Forwarded-Host") != "" ||
		headers.Get("X-Forwarded-Proto") != "" ||
		headers.Get("CF-Connecting-IP") != ""
}

func writeLocalError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
