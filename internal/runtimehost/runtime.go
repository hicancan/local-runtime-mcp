package runtimehost

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TunnelStatus describes the live state of one hot-pluggable remote wire.
type TunnelStatus struct {
	Name                string     `json:"name"`
	Configured          bool       `json:"configured"`
	State               string     `json:"state"`
	Address             string     `json:"address,omitempty"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	TTLRemainingSeconds int64      `json:"ttl_remaining_seconds,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

// TunnelController bridges live remote wire control and runtime telemetry into
// both the 9317 admin plane and the 9315 browser extension owner panel.
type TunnelController interface {
	Status() []TunnelStatus
	Up(name string, ttl time.Duration) ([]TunnelStatus, error)
	Down(name string) ([]TunnelStatus, error)
	AttachedClients() int
}

// CapabilitySummary summarizes the live tool catalog counts.
type CapabilitySummary struct {
	PageTools        int `json:"page_tools"`
	AppTools         int `json:"app_tools"`
	PendingProposals int `json:"pending_proposals"`
}

// Host owns the machine-local dependencies shared by every connection plane.
// Connections only determine how requests reach this one runtime.
type Host struct {
	httpServer        *mcpserver.Managed
	openaiServer      *mcpserver.Managed
	localServer       *mcpserver.Managed
	bridge            *browser.Bridge
	controller        computer.Controller
	processes         *runtimeprocess.Manager
	registry          *capability.Registry
	grants            *grantStore
	localCapabilities http.Handler
	startedAt         time.Time
	cancel            context.CancelFunc
	closeOnce         sync.Once
	closeErr          error
}

func Start(ctx context.Context, cfg *config.Config) (*Host, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("runtime configuration is required")
	}
	if err := config.ValidateBrowser(cfg.Browser); err != nil {
		return nil, err
	}
	if err := config.ValidateLocal(cfg.Local, len(cfg.Grants.Clients) > 0); err != nil {
		return nil, err
	}
	if err := validateGrantsAndCredentials(cfg); err != nil {
		return nil, err
	}
	// The Host keeps resource transport alive until its ordered Close.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	bridge, err := browser.Start(ctx, cfg.Browser)
	if err != nil {
		cancel()
		return nil, err
	}
	registry, err := capability.Load(cfg.Capabilities.Directory, bridge)
	if err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}
	if err := validateCapabilities(cfg, registry); err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}

	var localCapabilities http.Handler
	if len(cfg.Grants.Clients) > 0 {
		localCapabilities = localCapabilitiesHandler(cfg.Grants.Clients, registry, bridge.Status)
	}

	controller := computer.New(ctx)
	bridge.SetDesktopGate(controller.BeginDesktopOperation)
	processes := runtimeprocess.NewManager(ctx)
	fs := filesystem.New()

	baseDeps := mcpserver.Dependencies{
		Process:      processes,
		Filesystem:   fs,
		Browser:      bridge,
		Computer:     controller,
		Capabilities: registry,
	}
	httpDeps := baseDeps
	httpDeps.LocalApps = false
	httpDeps.Filter = remoteFilter(cfg.Grants.Remote.HTTP)
	httpServer := mcpserver.NewManaged(httpDeps)

	openaiDeps := baseDeps
	openaiDeps.LocalApps = false
	openaiDeps.Filter = remoteFilter(cfg.Grants.Remote.OpenAI)
	openaiServer := mcpserver.NewManaged(openaiDeps)

	localDeps := baseDeps
	localDeps.LocalApps = true
	localServer := mcpserver.NewManaged(localDeps)

	// Websites the owner lends machine tools to reach them through an unfiltered
	// in-process MCP session (built-in machine tools only, never LocalApps).
	machineDeps := baseDeps
	machineDeps.LocalApps = false
	machineServer := mcpserver.New(machineDeps)
	machine, err := connectMachineTools(ctx, machineServer)
	if err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}
	grants := newGrantStore(cfg.Grants.Web, cfg.Grants.Remote, cfg.Path, registry, machine, controller, func(target string, rg config.RemoteGrant) {
		switch target {
		case "openai":
			openaiServer.SetFilter(remoteFilter(rg))
		case "http":
			httpServer.SetFilter(remoteFilter(rg))
		}
	})
	if err := grants.checkAll(grants.bySite); err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}
	if err := grants.checkRemote(cfg.Grants.Remote.OpenAI); err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}
	if err := grants.checkRemote(cfg.Grants.Remote.HTTP); err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}
	bridge.SetWebHandler(webHandler(grants, registry))
	bridge.SetGrantHandler(grants.handle)
	return &Host{
		httpServer:        httpServer,
		openaiServer:      openaiServer,
		localServer:       localServer,
		bridge:            bridge,
		controller:        controller,
		processes:         processes,
		registry:          registry,
		grants:            grants,
		localCapabilities: localCapabilities,
		startedAt:         time.Now(),
		cancel:            cancel,
	}, nil
}

// Server returns the public HTTP / Cloudflare remote MCP Server (excludes LocalApps).
func (h *Host) Server() *mcp.Server { return h.httpServer.Server }

// RemoteServer returns the remote-safe MCP Server bound to target ("openai" or "http").
func (h *Host) RemoteServer(target string) *mcp.Server {
	if target == "openai" {
		return h.openaiServer.Server
	}
	return h.httpServer.Server
}

// LocalServer returns the same-host MCP Server (includes LocalApps).
func (h *Host) LocalServer() *mcp.Server { return h.localServer.Server }

// LocalCapabilitiesHandler serves /capabilities/* only on the dedicated local-only listener.
func (h *Host) LocalCapabilitiesHandler() http.Handler { return h.localCapabilities }

// SetTunnelController attaches the daemon's dynamic ingress controller so both
// the local admin API and the browser extension panel share live tunnel state.
func (h *Host) SetTunnelController(ctrl TunnelController) {
	if h == nil || h.grants == nil {
		return
	}
	h.grants.setTunnels(ctrl)
}

// SetRemoteGrant updates and persists a remote wire's tool grant and live-syncs its MCP server.
func (h *Host) SetRemoteGrant(target string, grant config.RemoteGrant) (config.RemoteGrants, error) {
	if err := h.grants.setRemote(target, grant); err != nil {
		return config.RemoteGrants{}, err
	}
	return h.grants.remoteCopy(), nil
}

// RemoteGrants returns the current remote wire grants.
func (h *Host) RemoteGrants() config.RemoteGrants { return h.grants.remoteCopy() }

// BrowserStatus returns the current status of connected browser profiles.
func (h *Host) BrowserStatus() browser.Status { return h.bridge.Status() }

// DesktopStatus returns the current desktop physical control status.
func (h *Host) DesktopStatus(ctx context.Context) computer.ControlStatus {
	status, err := h.controller.Control(ctx, computer.ControlOptions{Kind: "status"})
	if err != nil {
		return computer.ControlStatus{Status: "unavailable"}
	}
	return status
}

// StopDesktop revokes active desktop physical control immediately.
func (h *Host) StopDesktop() { h.controller.LocalStop() }

// CapabilitiesSummary returns counts of loaded Web MCP tools, App MCP tools, and proposals.
func (h *Host) CapabilitiesSummary() CapabilitySummary {
	return CapabilitySummary{
		PageTools:        len(h.registry.List()),
		AppTools:         len(h.registry.AppDefinitions()),
		PendingProposals: len(h.registry.Proposals()),
	}
}

// StartedAt returns the timestamp when the Host started.
func (h *Host) StartedAt() time.Time { return h.startedAt }

func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		h.cancel()
		h.closeErr = errors.Join(h.bridge.Close(ctx), h.controller.Close(), h.processes.Close())
	})
	return h.closeErr
}

func remoteFilter(g config.RemoteGrant) mcpserver.ToolFilter {
	if !g.ReadOnly && len(g.Tools) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(g.Tools))
	for _, name := range g.Tools {
		allowed[name] = true
	}
	return func(name string, readOnly, destructive bool) bool {
		if g.ReadOnly && (!readOnly || destructive) {
			return false
		}
		if len(allowed) > 0 && !allowed[name] {
			return false
		}
		return true
	}
}
