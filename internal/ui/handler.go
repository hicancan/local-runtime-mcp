// Package ui serves the embedded local-only Sovereign Activity & Audit Studio
// mounted strictly under 127.0.0.1:9317/ui with single-use bootstrap auth.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
)

//go:embed assets/*
var embeddedAssets embed.FS

const (
	cookieName     = "lrmcp_ui"
	bootstrapTTL   = 60 * time.Second
	maxActiveCodes = 32
	studioCSP      = "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'"
)

// StatusSnapshot is the runtime summary rendered in the Studio UI control bar.
type StatusSnapshot struct {
	Version        string                        `json:"version"`
	PID            int                           `json:"pid"`
	StartedAt      time.Time                     `json:"started_at"`
	UptimeSeconds  int64                         `json:"uptime_seconds"`
	LocalListen    string                        `json:"local_listen"`
	AttachedStdio  int                           `json:"attached_stdio"`
	Browser        browser.Status                `json:"browser"`
	DesktopControl computer.ControlStatus        `json:"desktop_control"`
	Capabilities   runtimehost.CapabilitySummary `json:"capabilities"`
	Tunnels        []runtimehost.TunnelStatus    `json:"tunnels"`
	RemoteGrants   config.RemoteGrants           `json:"remote_grants"`
	WebGrants      []config.WebGrant             `json:"web_grants"`
	AuditStats     audit.Stats                   `json:"audit_stats"`
}

// Backend supplies the live Host and IngressManager actions to the Studio UI.
type Backend struct {
	Audit           *audit.Store
	Status          func(ctx context.Context) StatusSnapshot
	TunnelUp        func(name string, ttl time.Duration) ([]runtimehost.TunnelStatus, error)
	TunnelDown      func(name string) ([]runtimehost.TunnelStatus, error)
	SetRemoteGrant  func(target string, grant config.RemoteGrant) (config.RemoteGrants, error)
	StopDesktop     func()
	Proposals       func() []capability.Proposal
	ApproveProposal func(name string) error
	RejectProposal  func(name string) error
	KickSession     func(sessionID string) bool
	ConnectBrowser  func(ctx context.Context, browserID string) (browser.InstanceStatus, error)
}

// Studio serves /ui and /ui/api/* behind single-use bootstrap cookie auth.
type Studio struct {
	backend       Backend
	sessionSecret string
	assets        fs.FS
	mux           *http.ServeMux

	codeMu sync.Mutex
	codes  map[string]time.Time
}

// New creates a Studio handler with a fresh in-memory session secret.
func New(backend Backend) (*Studio, error) {
	sub, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	s := &Studio{
		backend:       backend,
		sessionSecret: hex.EncodeToString(raw),
		assets:        sub,
		mux:           http.NewServeMux(),
		codes:         map[string]time.Time{},
	}
	s.registerRoutes()
	return s, nil
}

// MintBootstrapURL generates a single-use 60-second bootstrap URL for opening /ui.
func (s *Studio) MintBootstrapURL(localAddr string) (string, error) {
	if s == nil || localAddr == "" {
		return "", errors.New("studio UI address unavailable")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := hex.EncodeToString(raw)
	now := time.Now()

	s.codeMu.Lock()
	for k, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, k)
		}
	}
	if len(s.codes) >= maxActiveCodes {
		s.codes = map[string]time.Time{}
	}
	s.codes[code] = now.Add(bootstrapTTL)
	s.codeMu.Unlock()

	return fmt.Sprintf("http://%s/ui/auth?code=%s", localAddr, code), nil
}

func (s *Studio) consumeCode(provided string) bool {
	if len(provided) != 48 {
		return false
	}
	now := time.Now()
	s.codeMu.Lock()
	defer s.codeMu.Unlock()
	matchedKey := ""
	for k, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, k)
			continue
		}
		if len(k) == len(provided) && subtle.ConstantTimeCompare([]byte(k), []byte(provided)) == 1 {
			matchedKey = k
		}
	}
	if matchedKey == "" {
		return false
	}
	delete(s.codes, matchedKey)
	return true
}

func (s *Studio) hasValidCookie(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != len(s.sessionSecret) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.sessionSecret)) == 1
}

// ServeHTTP enforces single-use bootstrap auth, cookie validation, CSP, and CSRF checks.
func (s *Studio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")

	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.URL.Path == "/ui/favicon.svg" || r.URL.Path == "/favicon.svg" || r.URL.Path == "/favicon.ico") {
		s.serveAsset(w, "favicon.svg")
		return
	}

	if r.URL.Path == "/ui/auth" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.consumeCode(r.URL.Query().Get("code")) {
			writeUnauthorizedHTML(w)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    s.sessionSecret,
			Path:     "/ui",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}

	if !s.hasValidCookie(r) {
		if strings.HasPrefix(r.URL.Path, "/ui/api/") {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "Open the Studio via `lrmcp ui` or the browser extension button.")
			return
		}
		writeUnauthorizedHTML(w)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-LRMCP-UI") != "1" {
			writeJSONError(w, http.StatusForbidden, "csrf_rejected", "Missing X-LRMCP-UI header.")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !strings.EqualFold(origin, "http://"+r.Host) {
			writeJSONError(w, http.StatusForbidden, "csrf_rejected", "Cross-origin state mutation is forbidden.")
			return
		}
	}

	s.mux.ServeHTTP(w, r)
}

func (s *Studio) registerRoutes() {
	s.mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ui/" {
			s.serveAsset(w, strings.TrimPrefix(r.URL.Path, "/ui/"))
			return
		}
		s.serveAsset(w, "index.html")
	})

	s.mux.HandleFunc("GET /ui/api/status", func(w http.ResponseWriter, r *http.Request) {
		if s.backend.Status == nil {
			writeJSON(w, http.StatusOK, StatusSnapshot{})
			return
		}
		writeJSON(w, http.StatusOK, s.backend.Status(r.Context()))
	})

	s.mux.HandleFunc("GET /ui/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		limit := parseIntQuery(r, "limit", 120, 1, 500)
		sessions, err := s.backend.Audit.ListSessions(limit)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "audit_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
	})

	s.mux.HandleFunc("GET /ui/api/events", func(w http.ResponseWriter, r *http.Request) {
		f := audit.EventFilter{
			SessionID: r.URL.Query().Get("session_id"),
			AgentID:   r.URL.Query().Get("agent_id"),
			Wire:      r.URL.Query().Get("wire"),
			Kind:      r.URL.Query().Get("kind"),
			Status:    r.URL.Query().Get("status"),
			BeforeID:  parseInt64Query(r, "before_id"),
			AfterID:   parseInt64Query(r, "after_id"),
			Limit:     parseIntQuery(r, "limit", 250, 1, 1000),
		}
		events, err := s.backend.Audit.ListEvents(f)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "audit_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events})
	})

	s.mux.HandleFunc("GET /ui/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		limit := parseIntQuery(r, "limit", 80, 1, 200)
		events, err := s.backend.Audit.Search(q, limit)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "audit_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"query": q, "events": events})
	})

	s.mux.HandleFunc("GET /ui/api/blobs/{hash}", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		data, err := s.backend.Audit.GetBlob(hash)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(data))
		w.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = w.Write(data)
	})

	s.mux.HandleFunc("GET /ui/api/export", func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session_id")
		filename := "lrmcp-audit.jsonl"
		if sid != "" {
			filename = "lrmcp-" + audit.SanitizeID(strings.ReplaceAll(sid, ":", "-")) + ".jsonl"
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		_ = s.backend.Audit.ExportJSONL(w, sid)
	})

	s.mux.HandleFunc("GET /ui/api/stream", func(w http.ResponseWriter, r *http.Request) {
		ServeSSE(w, r, s.backend.Audit)
	})

	s.mux.HandleFunc("GET /ui/api/proposals", func(w http.ResponseWriter, _ *http.Request) {
		list := []capability.Proposal{}
		if s.backend.Proposals != nil {
			list = s.backend.Proposals()
		}
		writeJSON(w, http.StatusOK, map[string]any{"proposals": list})
	})

	s.mux.HandleFunc("POST /ui/api/proposals/approve", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(w, r, &req); err != nil || req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "Proposal name is required.")
			return
		}
		if err := s.backend.ApproveProposal(req.Name); err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, "approve_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proposals": s.backend.Proposals()})
	})

	s.mux.HandleFunc("POST /ui/api/proposals/reject", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(w, r, &req); err != nil || req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "Proposal name is required.")
			return
		}
		if err := s.backend.RejectProposal(req.Name); err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, "reject_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proposals": s.backend.Proposals()})
	})

	s.mux.HandleFunc("POST /ui/api/tunnels/up", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string `json:"name"`
			TTLSeconds int64  `json:"ttl_seconds"`
		}
		if err := decodeJSON(w, r, &req); err != nil || req.Name == "" || req.TTLSeconds < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "Wire name and non-negative ttl_seconds required.")
			return
		}
		tunnels, err := s.backend.TunnelUp(req.Name, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, "tunnel_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tunnels": tunnels})
	})

	s.mux.HandleFunc("POST /ui/api/tunnels/down", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(w, r, &req); err != nil || req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "Wire name required.")
			return
		}
		tunnels, err := s.backend.TunnelDown(req.Name)
		if err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, "tunnel_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tunnels": tunnels})
	})

	s.mux.HandleFunc("POST /ui/api/remote-grants", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target string             `json:"target"`
			Grant  config.RemoteGrant `json:"grant"`
		}
		if err := decodeJSON(w, r, &req); err != nil || req.Target == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "Target and grant required.")
			return
		}
		updated, err := s.backend.SetRemoteGrant(req.Target, req.Grant)
		if err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, "grant_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"remote_grants": updated})
	})

	s.mux.HandleFunc("POST /ui/api/desktop/stop", func(w http.ResponseWriter, _ *http.Request) {
		if s.backend.StopDesktop != nil {
			s.backend.StopDesktop()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	s.mux.HandleFunc("POST /ui/api/sessions/kick", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
		}
		if err := decodeJSON(w, r, &req); err != nil || req.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "session_id is required.")
			return
		}
		disconnected := false
		if s.backend.KickSession != nil {
			disconnected = s.backend.KickSession(req.SessionID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disconnected": disconnected})
	})

	s.mux.HandleFunc("POST /ui/api/browser/connect", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			BrowserID string `json:"browser_id"`
		}
		_ = decodeJSON(w, r, &req)
		if s.backend.ConnectBrowser == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "browser_unavailable", "Browser CDP engine is unavailable.")
			return
		}
		inst, err := s.backend.ConnectBrowser(r.Context(), req.BrowserID)
		if err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, "browser_connect_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"instance": inst})
	})
}

func (s *Studio) serveAsset(w http.ResponseWriter, name string) {
	if strings.Contains(name, "..") || strings.ContainsAny(name, "\\\x00") {
		http.NotFound(w, nil)
		return
	}
	data, err := fs.ReadFile(s.assets, name)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Security-Policy", studioCSP)
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	_, _ = w.Write(data)
}

// ServeSSE streams live audit packets as Server-Sent Events to either /ui/api/stream
// or /admin/audit/stream.
func ServeSSE(w http.ResponseWriter, r *http.Request, store *audit.Store) {
	flusher, ok := w.(http.Flusher)
	if !ok || store == nil {
		writeJSONError(w, http.StatusInternalServerError, "stream_unavailable", "Streaming is not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	ch, unsub := store.Subscribe()
	defer unsub()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		case pkt, open := <-ch:
			if !open {
				return
			}
			b, err := json.Marshal(pkt)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", pkt.Type, b)
			flusher.Flush()
		}
	}
}

func writeUnauthorizedHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", studioCSP)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>lrmcp studio — locked</title><link rel="icon" type="image/svg+xml" href="/ui/favicon.svg"><style>body{margin:0;min-height:100vh;display:grid;place-items:center;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f8f9fa;color:#111827}.card{max-width:420px;padding:28px 32px;background:#fff;border:1px solid #e5e7eb;border-radius:14px;box-shadow:0 2px 10px rgba(0,0,0,.04)}.head{display:flex;align-items:center;gap:10px;margin-bottom:10px}.head img{width:26px;height:26px;display:block}h1{margin:0;font-size:18px;font-weight:600}p{margin:0 0 12px;font-size:14px;line-height:1.6;color:#4b5563}code{background:#f3f4f6;padding:2px 6px;border-radius:5px;font-size:13px;color:#111827}</style></head><body><div class="card"><div class="head"><img src="/ui/favicon.svg" alt="lrmcp"><h1>Local Runtime Studio</h1></div><p>This local-only dashboard requires a single-use owner session from your machine.</p><p>Run <code>lrmcp ui</code> in your terminal or click <strong>Studio</strong> in the <code>L</code> browser extension.</p></div></body></html>`)
}

func parseIntQuery(r *http.Request, name string, def, min, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func parseInt64Query(r *http.Request, name string) int64 {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
