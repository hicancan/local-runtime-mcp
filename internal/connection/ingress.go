package connection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
	cloudflaretunnel "github.com/hicancan/local-runtime-mcp/internal/tunnel/cloudflare"
	openaitunnel "github.com/hicancan/local-runtime-mcp/internal/tunnel/openai"
	"github.com/openai/tunnel-client/pkg/runtimeconfig"
)

type wireEntry struct {
	name      string
	state     string // "stopped", "active", "error"
	address   string
	startedAt *time.Time
	expiresAt *time.Time
	lastError string
	gen       uint64
	cancel    context.CancelFunc
	done      chan struct{}
	timer     *time.Timer
}

// IngressManager dynamically attaches and detaches remote wires ("openai",
// "cloudflare", "http") on top of one running Host without restarting it or
// disrupting local clients or the browser bridge.
type IngressManager struct {
	rootCtx  context.Context
	cfg      *config.Config
	host     *runtimehost.Host
	log      io.Writer
	attached func() int

	mu            sync.Mutex
	closed        bool
	wires         map[string]*wireEntry
	httpExplicit  bool
	httpOwnedByCF bool

	// Injectable runners for unit testing without external network or binaries.
	runOpenAI     func(context.Context, *config.Config, *runtimehost.Host) error
	runCloudflare func(context.Context, *config.Config, io.Writer) error
}

func newIngressManager(rootCtx context.Context, cfg *config.Config, host *runtimehost.Host, log io.Writer, attached func() int) *IngressManager {
	if log == nil {
		log = io.Discard
	}
	return &IngressManager{
		rootCtx:  rootCtx,
		cfg:      cfg,
		host:     host,
		log:      log,
		attached: attached,
		wires: map[string]*wireEntry{
			"openai":     {name: "openai", state: "stopped"},
			"cloudflare": {name: "cloudflare", state: "stopped"},
			"http":       {name: "http", state: "stopped"},
		},
		runOpenAI: func(ctx context.Context, c *config.Config, h *runtimehost.Host) error {
			return openaitunnel.Run(ctx, h.RemoteServer("openai"), openaitunnel.Config{
				TunnelID: c.OpenAI.TunnelID,
				APIKey:   c.OpenAI.APIKey,
			})
		},
		runCloudflare: func(ctx context.Context, c *config.Config, out io.Writer) error {
			return cloudflaretunnel.Run(ctx, cloudflaretunnel.Config{
				Binary: c.Cloudflare.Binary,
				Token:  c.Cloudflare.TunnelToken,
				Stdout: out,
				Stderr: out,
			})
		},
	}
}

func (m *IngressManager) AttachedClients() int {
	if m == nil || m.attached == nil {
		return 0
	}
	return m.attached()
}

func (m *IngressManager) isConfigured(name string) bool {
	switch name {
	case "openai":
		return runtimeconfig.ValidateTunnelID(m.cfg.OpenAI.TunnelID) == nil &&
			runtimeconfig.ValidateControlPlaneAPIKey(m.cfg.OpenAI.APIKey) == nil
	case "cloudflare":
		return m.cfg.HTTP.PublicHost != "" && m.cfg.Cloudflare.TunnelToken != "" && m.httpConfigured()
	case "http":
		return m.httpConfigured()
	default:
		return false
	}
}

func (m *IngressManager) httpConfigured() bool {
	cfg, err := publicHTTPConfig(m.cfg)
	if err != nil {
		return false
	}
	if cfg.OAuth != nil {
		return cfg.OAuth.OwnerToken != "" && len(cfg.OAuth.Clients) > 0
	}
	return len(cfg.BearerToken) >= 32 || len(cfg.RemoteTokens) > 0
}

// Status returns a deterministic snapshot of all remote wires.
func (m *IngressManager) Status() []runtimehost.TunnelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

func (m *IngressManager) statusLocked() []runtimehost.TunnelStatus {
	order := []string{"openai", "cloudflare", "http"}
	out := make([]runtimehost.TunnelStatus, 0, len(order))
	for _, name := range order {
		w := m.wires[name]
		item := runtimehost.TunnelStatus{
			Name:       name,
			Configured: m.isConfigured(name),
			State:      w.state,
			Address:    w.address,
			LastError:  w.lastError,
		}
		if w.startedAt != nil {
			t := *w.startedAt
			item.StartedAt = &t
		}
		if w.expiresAt != nil && w.state == "active" {
			t := *w.expiresAt
			item.ExpiresAt = &t
			rem := int64(time.Until(t).Seconds())
			if rem < 0 {
				rem = 0
			}
			item.TTLRemainingSeconds = rem
		}
		out = append(out, item)
	}
	return out
}

// Up hot-plugs a remote wire ("openai", "cloudflare", or "http") with an optional
// automatic expiration lease (ttl == 0 means no automatic expiry).
func (m *IngressManager) Up(name string, ttl time.Duration) ([]runtimehost.TunnelStatus, error) {
	if ttl < 0 {
		return nil, errors.New("ttl cannot be negative")
	}
	switch name {
	case "openai", "cloudflare", "http":
	default:
		return nil, fmt.Errorf("unknown remote wire %q; choose openai, cloudflare, or http", name)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("daemon is shutting down")
	}

	// If already active, refresh TTL lease idempotently.
	w := m.wires[name]
	if w.state == "active" {
		if name == "http" {
			m.httpExplicit = true
		}
		m.setLeaseLocked(w, ttl)
		st := m.statusLocked()
		m.mu.Unlock()
		return st, nil
	}
	m.mu.Unlock()

	switch name {
	case "openai":
		if err := runtimeconfig.ValidateTunnelID(m.cfg.OpenAI.TunnelID); err != nil {
			return nil, fmt.Errorf("openai tunnel not configured: %w", err)
		}
		if err := runtimeconfig.ValidateControlPlaneAPIKey(m.cfg.OpenAI.APIKey); err != nil {
			return nil, fmt.Errorf("openai api_key not configured: %w", err)
		}
		return m.startWire("openai", "", ttl, func(ctx context.Context) error {
			return m.runOpenAI(ctx, m.cfg, m.host)
		})

	case "http":
		return m.startHTTP(ttl, true)

	case "cloudflare":
		if m.cfg.HTTP.PublicHost == "" || m.cfg.Cloudflare.TunnelToken == "" {
			return nil, errors.New("Cloudflare requires http.public_host and cloudflare.tunnel_token")
		}
		if _, err := cloudflaretunnel.FindBinary(m.cfg.Cloudflare.Binary); err != nil {
			return nil, err
		}
		// Ensure public loopback HTTP origin (9316) is listening before cloudflared starts.
		m.mu.Lock()
		httpAlreadyActive := m.wires["http"].state == "active"
		m.mu.Unlock()
		if !httpAlreadyActive {
			if _, err := m.startHTTP(0, false); err != nil {
				return nil, fmt.Errorf("start public HTTP origin for Cloudflare: %w", err)
			}
		}
		st, err := m.startWire("cloudflare", m.cfg.HTTP.PublicHost, ttl, func(ctx context.Context) error {
			return m.runCloudflare(ctx, m.cfg, m.log)
		})
		if err != nil {
			if !httpAlreadyActive {
				_, _ = m.Down("http")
			}
			return nil, err
		}
		return st, nil
	}
	return nil, errors.New("unreachable")
}

func (m *IngressManager) startHTTP(ttl time.Duration, explicit bool) ([]runtimehost.TunnelStatus, error) {
	httpCfg, err := publicHTTPConfig(m.cfg)
	if err != nil {
		return nil, err
	}
	srv, err := transport.NewHTTP(m.host.RemoteServer("http"), httpCfg)
	if err != nil {
		return nil, err
	}
	addr := srv.Address()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = srv.Run(cancelCtx)
		return nil, errors.New("daemon is shutting down")
	}
	if m.wires["http"].state == "active" {
		if explicit {
			m.httpExplicit = true
		}
		m.setLeaseLocked(m.wires["http"], ttl)
		st := m.statusLocked()
		m.mu.Unlock()
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = srv.Run(cancelCtx)
		return st, nil
	}
	if explicit {
		m.httpExplicit = true
		m.httpOwnedByCF = false
	} else {
		m.httpOwnedByCF = true
	}
	m.mu.Unlock()
	fmt.Fprintf(m.log, "[http] Streamable HTTP listening on http://%s/mcp (auth=%s)\n", addr, m.cfg.HTTP.Auth)
	return m.startWire("http", addr, ttl, srv.Run)
}

func (m *IngressManager) startWire(name, address string, ttl time.Duration, run func(context.Context) error) ([]runtimehost.TunnelStatus, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("daemon is shutting down")
	}
	w := m.wires[name]
	if w.state == "active" {
		m.setLeaseLocked(w, ttl)
		st := m.statusLocked()
		m.mu.Unlock()
		return st, nil
	}
	w.gen++
	gen := w.gen
	wireCtx, cancel := context.WithCancel(m.rootCtx)
	done := make(chan struct{})
	now := time.Now()
	w.state = "active"
	w.address = address
	w.startedAt = &now
	w.lastError = ""
	w.cancel = cancel
	w.done = done
	m.setLeaseLocked(w, ttl)
	st := m.statusLocked()
	m.mu.Unlock()

	fmt.Fprintf(m.log, "[%s] remote wire attached\n", name)
	if m.host != nil && m.host.Audit() != nil {
		m.host.Audit().RecordSystem("system", fmt.Sprintf("Attached remote wire %s", name), map[string]any{"wire": name, "address": address, "ttl_seconds": int64(ttl.Seconds())})
	}
	go func() {
		defer close(done)
		err := normalizeCancellation(run(wireCtx))
		var auditSummary string
		var auditDetail map[string]any
		m.mu.Lock()
		if w.gen == gen {
			if w.timer != nil {
				w.timer.Stop()
				w.timer = nil
			}
			w.expiresAt = nil
			w.cancel = nil
			w.done = nil
			if err != nil {
				w.state = "error"
				w.lastError = err.Error()
				fmt.Fprintf(m.log, "[%s] remote wire stopped with error: %v\n", name, err)
				auditSummary = fmt.Sprintf("Remote wire %s error: %v", name, err)
				auditDetail = map[string]any{"wire": name, "error": err.Error()}
			} else {
				w.state = "stopped"
				w.lastError = ""
				fmt.Fprintf(m.log, "[%s] remote wire detached\n", name)
				auditSummary = fmt.Sprintf("Detached remote wire %s", name)
				auditDetail = map[string]any{"wire": name}
			}
			w.address = ""
			w.startedAt = nil
			if name == "http" {
				m.httpExplicit = false
				m.httpOwnedByCF = false
			}
		}
		m.mu.Unlock()
		if auditSummary != "" && m.host != nil && m.host.Audit() != nil {
			m.host.Audit().RecordSystem("system", auditSummary, auditDetail)
		}
	}()

	return st, nil
}

func (m *IngressManager) setLeaseLocked(w *wireEntry, ttl time.Duration) {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if ttl <= 0 {
		w.expiresAt = nil
		return
	}
	exp := time.Now().Add(ttl)
	w.expiresAt = &exp
	gen := w.gen
	name := w.name
	w.timer = time.AfterFunc(ttl, func() {
		m.mu.Lock()
		same := m.wires[name].gen == gen && m.wires[name].state == "active"
		m.mu.Unlock()
		if same {
			fmt.Fprintf(m.log, "[%s] lease expired; detaching remote wire\n", name)
			_, _ = m.Down(name)
		}
	})
}

// Down hot-unplugs one remote wire ("openai", "cloudflare", "http") or "all".
func (m *IngressManager) Down(name string) ([]runtimehost.TunnelStatus, error) {
	switch name {
	case "all":
		_, _ = m.downOne("openai")
		_, _ = m.downOne("cloudflare")
		_, _ = m.downOne("http")
		return m.Status(), nil
	case "openai":
		return m.downOne("openai")
	case "cloudflare":
		_, err := m.downOne("cloudflare")
		m.mu.Lock()
		dropHTTP := m.httpOwnedByCF && !m.httpExplicit
		m.mu.Unlock()
		if dropHTTP {
			_, _ = m.downOne("http")
		}
		return m.Status(), err
	case "http":
		// Detach Cloudflare first if it relies on the public HTTP listener.
		_, _ = m.downOne("cloudflare")
		m.mu.Lock()
		m.httpExplicit = false
		m.httpOwnedByCF = false
		m.mu.Unlock()
		return m.downOne("http")
	default:
		return nil, fmt.Errorf("unknown remote wire %q; choose openai, cloudflare, http, or all", name)
	}
}

func (m *IngressManager) downOne(name string) ([]runtimehost.TunnelStatus, error) {
	m.mu.Lock()
	w := m.wires[name]
	cancel := w.cancel
	done := w.done
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.expiresAt = nil
	if w.state == "error" {
		w.state = "stopped"
		w.lastError = ""
	}
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(8 * time.Second):
		}
	}
	return m.Status(), nil
}

// Close cleanly detaches every active remote wire during daemon shutdown.
func (m *IngressManager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	_, _ = m.Down("all")
}
