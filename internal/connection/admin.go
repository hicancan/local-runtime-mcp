package connection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
	"github.com/hicancan/local-runtime-mcp/internal/ui"
)

// DaemonStatus is the live snapshot returned by GET /admin/status and `lrmcp status`.
type DaemonStatus struct {
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
	Audit          audit.Stats                   `json:"audit"`
}

func buildStatus(ctx context.Context, localAddr func() string, host *runtimehost.Host, ingress *IngressManager) DaemonStatus {
	started := host.StartedAt()
	uptime := int64(time.Since(started).Seconds())
	if uptime < 0 {
		uptime = 0
	}
	auditStats, _ := host.Audit().Stats()
	return DaemonStatus{
		Version:        mcpserver.Version,
		PID:            os.Getpid(),
		StartedAt:      started.UTC(),
		UptimeSeconds:  uptime,
		LocalListen:    localAddr(),
		AttachedStdio:  ingress.AttachedClients(),
		Browser:        host.BrowserStatus(),
		DesktopControl: host.DesktopStatus(ctx),
		Capabilities:   host.CapabilitiesSummary(),
		Tunnels:        ingress.Status(),
		RemoteGrants:   host.RemoteGrants(),
		Audit:          auditStats,
	}
}

func newStudio(
	localAddr func() string,
	host *runtimehost.Host,
	ingress *IngressManager,
	kickSession func(string) bool,
) (*ui.Studio, error) {
	return ui.New(ui.Backend{
		Audit: host.Audit(),
		Status: func(ctx context.Context) ui.StatusSnapshot {
			st := buildStatus(ctx, localAddr, host, ingress)
			return ui.StatusSnapshot{
				Version:        st.Version,
				PID:            st.PID,
				StartedAt:      st.StartedAt,
				UptimeSeconds:  st.UptimeSeconds,
				LocalListen:    st.LocalListen,
				AttachedStdio:  st.AttachedStdio,
				Browser:        st.Browser,
				DesktopControl: st.DesktopControl,
				Capabilities:   st.Capabilities,
				Tunnels:        st.Tunnels,
				RemoteGrants:   st.RemoteGrants,
				WebGrants:      host.WebGrants(),
				AuditStats:     st.Audit,
			}
		},
		TunnelUp:        ingress.Up,
		TunnelDown:      ingress.Down,
		SetRemoteGrant:  host.SetRemoteGrant,
		StopDesktop:     host.StopDesktop,
		Proposals:       host.Proposals,
		ApproveProposal: host.ApproveProposal,
		RejectProposal:  host.RejectProposal,
		KickSession:     kickSession,
	})
}

func newAdminHandler(
	localAddr func() string,
	host *runtimehost.Host,
	ingress *IngressManager,
	studio *ui.Studio,
	kickSession func(string) bool,
	shutdown func(),
) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /admin/status", func(w http.ResponseWriter, r *http.Request) {
		writeAdminJSON(w, http.StatusOK, buildStatus(r.Context(), localAddr, host, ingress))
	})

	mux.HandleFunc("POST /admin/ui/bootstrap", func(w http.ResponseWriter, _ *http.Request) {
		if studio == nil {
			writeAdminError(w, http.StatusNotFound, "ui_unavailable", "Local Studio UI is not enabled.")
			return
		}
		url, err := studio.MintBootstrapURL(localAddr())
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "bootstrap_failed", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"url": url})
	})

	mux.HandleFunc("GET /admin/audit/sessions", func(w http.ResponseWriter, r *http.Request) {
		limit := adminIntQuery(r, "limit", 100, 1, 500)
		sessions, err := host.Audit().ListSessions(limit)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "audit_error", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
	})

	mux.HandleFunc("GET /admin/audit/events", func(w http.ResponseWriter, r *http.Request) {
		compact := adminBoolQuery(r, "compact")
		events, err := host.Audit().ListEvents(audit.EventFilter{
			SessionID: r.URL.Query().Get("session_id"),
			AgentID:   r.URL.Query().Get("agent_id"),
			Wire:      r.URL.Query().Get("wire"),
			Kind:      r.URL.Query().Get("kind"),
			Status:    r.URL.Query().Get("status"),
			Limit:     adminIntQuery(r, "limit", 100, 1, 1000),
			Compact:   compact,
		})
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "audit_error", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"events": events})
	})

	mux.HandleFunc("GET /admin/audit/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "Provide a positive integer event id.")
			return
		}
		ev, err := host.Audit().GetEvent(id)
		if errors.Is(err, sql.ErrNoRows) {
			writeAdminError(w, http.StatusNotFound, "not_found", "Audit event not found.")
			return
		}
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "audit_error", err.Error())
			return
		}
		resp := map[string]any{"event": ev}
		if ev.ImageBlob != "" {
			if p := host.Audit().BlobPath(ev.ImageBlob); p != "" {
				resp["blob_path"] = p
			}
		}
		writeAdminJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /admin/audit/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		limit := adminIntQuery(r, "limit", 80, 1, 200)
		compact := adminBoolQuery(r, "compact")
		events, err := host.Audit().SearchWithOptions(q, limit, compact)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "audit_error", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"query": q, "events": events})
	})

	mux.HandleFunc("GET /admin/audit/stream", func(w http.ResponseWriter, r *http.Request) {
		ui.ServeSSE(w, r, host.Audit())
	})

	mux.HandleFunc("GET /admin/audit/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		_ = host.Audit().ExportJSONL(w, r.URL.Query().Get("session_id"))
	})

	mux.HandleFunc("POST /admin/sessions/kick", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
		}
		if err := decodeAdminJSON(w, r, &req); err != nil || req.SessionID == "" {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "Provide session_id.")
			return
		}
		disconnected := false
		if kickSession != nil {
			disconnected = kickSession(req.SessionID)
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "disconnected": disconnected})
	})

	mux.HandleFunc("POST /admin/tunnels/up", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string `json:"name"`
			TTLSeconds int64  `json:"ttl_seconds"`
		}
		if err := decodeAdminJSON(w, r, &req); err != nil || req.Name == "" || req.TTLSeconds < 0 {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "Provide a wire name (openai, cloudflare, or http) and non-negative ttl_seconds.")
			return
		}
		tunnels, err := ingress.Up(req.Name, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			writeAdminError(w, http.StatusUnprocessableEntity, "tunnel_start_failed", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"tunnels": tunnels})
	})

	mux.HandleFunc("POST /admin/tunnels/down", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeAdminJSON(w, r, &req); err != nil || req.Name == "" {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "Provide a wire name (openai, cloudflare, http, or all).")
			return
		}
		tunnels, err := ingress.Down(req.Name)
		if err != nil {
			writeAdminError(w, http.StatusUnprocessableEntity, "tunnel_stop_failed", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"tunnels": tunnels})
	})

	mux.HandleFunc("POST /admin/remote-grants", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target string             `json:"target"`
			Grant  config.RemoteGrant `json:"grant"`
		}
		if err := decodeAdminJSON(w, r, &req); err != nil || req.Target == "" {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "Provide target (openai or http) and grant.")
			return
		}
		updated, err := host.SetRemoteGrant(req.Target, req.Grant)
		if err != nil {
			writeAdminError(w, http.StatusUnprocessableEntity, "invalid_remote_grant", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"remote_grants": updated})
	})

	mux.HandleFunc("POST /admin/desktop/stop", func(w http.ResponseWriter, _ *http.Request) {
		host.StopDesktop()
		writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /admin/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		writeAdminJSON(w, http.StatusOK, map[string]any{"shutting_down": true, "pid": os.Getpid()})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if shutdown != nil {
			go func() {
				time.Sleep(30 * time.Millisecond)
				shutdown()
			}()
		}
	})

	return mux
}

func adminBoolQuery(r *http.Request, name string) bool {
	switch r.URL.Query().Get(name) {
	case "1", "true", "TRUE", "yes":
		return true
	default:
		return false
	}
}

func adminIntQuery(r *http.Request, name string, def, min, max int) int {
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

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, target any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	writeAdminJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
