package connection

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
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
}

func newAdminHandler(
	localAddr func() string,
	host *runtimehost.Host,
	ingress *IngressManager,
	shutdown func(),
) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /admin/status", func(w http.ResponseWriter, r *http.Request) {
		started := host.StartedAt()
		uptime := int64(time.Since(started).Seconds())
		if uptime < 0 {
			uptime = 0
		}
		writeAdminJSON(w, http.StatusOK, DaemonStatus{
			Version:        mcpserver.Version,
			PID:            os.Getpid(),
			StartedAt:      started.UTC(),
			UptimeSeconds:  uptime,
			LocalListen:    localAddr(),
			AttachedStdio:  ingress.AttachedClients(),
			Browser:        host.BrowserStatus(),
			DesktopControl: host.DesktopStatus(r.Context()),
			Capabilities:   host.CapabilitiesSummary(),
			Tunnels:        ingress.Status(),
			RemoteGrants:   host.RemoteGrants(),
		})
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

// Unused import guard for context if needed by callers.
var _ context.Context
