package runtimehost

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/modelapi"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Host owns the machine-local dependencies shared by every connection plane.
// Connections only determine how requests reach this one runtime.
type Host struct {
	publicServer      *mcp.Server
	localServer       *mcp.Server
	bridge            *browser.Bridge
	controller        computer.Controller
	processes         *runtimeprocess.Manager
	localModels       http.Handler
	modelService      *modelapi.Service
	localCapabilities http.Handler
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
	// The Host keeps resource transport alive until its ordered Close, so a
	// caller's cancellation can stop/close owned conversations before the bridge.
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
	items := make([]modelapi.Model, 0, len(cfg.Models.Items))
	for _, m := range cfg.Models.Items {
		items = append(items, modelapi.Model{ID: m.ID, Title: m.Title, BrowserID: m.BrowserID, URL: m.URL, Start: m.Start, Read: m.Read, Stop: m.Stop, TimeoutSeconds: m.TimeoutSeconds})
	}
	models, err := modelapi.New(items, registry, bridge)
	if err == nil {
		err = validateCapabilities(cfg, registry)
	}
	if err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}

	var localModels, localCapabilities http.Handler
	if len(cfg.Grants.Clients) > 0 {
		localModels = &modelapi.HTTP{Service: models, Authorize: localModelAuthorizer(cfg.Grants.Clients)}
		localCapabilities = localCapabilitiesHandler(cfg.Grants.Clients, registry, bridge.Status)
	}

	controller := computer.New(ctx)
	bridge.SetDesktopGate(controller.BeginDesktopOperation)
	processes := runtimeprocess.NewManager(ctx)
	fs := filesystem.New()
	publicServer := mcpserver.New(mcpserver.Dependencies{Process: processes, Filesystem: fs, Browser: bridge, Computer: controller, Capabilities: registry, LocalApps: false})
	localServer := mcpserver.New(mcpserver.Dependencies{Process: processes, Filesystem: fs, Browser: bridge, Computer: controller, Capabilities: registry, LocalApps: true})

	// Websites the owner lends machine tools to reach them through the public Server
	// (built-in machine tools only, never local app bindings).
	machine, err := connectMachineTools(ctx, publicServer)
	if err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}
	grants := newGrantStore(cfg.Grants.Web, cfg.Path, registry, models.List(), machine)
	if err := grants.checkAll(grants.bySite); err != nil {
		cancel()
		_ = bridge.Close(context.Background())
		return nil, err
	}
	bridge.SetWebHandler(webHandler(grants, registry, models))
	bridge.SetGrantHandler(grants.handle)
	return &Host{
		publicServer:      publicServer,
		localServer:       localServer,
		bridge:            bridge,
		controller:        controller,
		processes:         processes,
		localModels:       localModels,
		modelService:      models,
		localCapabilities: localCapabilities,
		cancel:            cancel,
	}, nil
}

// Server returns the remote-safe MCP Server (excludes LocalApps).
func (h *Host) Server() *mcp.Server { return h.publicServer }

// LocalServer returns the same-host MCP Server (includes LocalApps).
func (h *Host) LocalServer() *mcp.Server { return h.localServer }

// LocalModelsHandler serves /v1/* only on the dedicated local-only listener.
func (h *Host) LocalModelsHandler() http.Handler { return h.localModels }

// LocalCapabilitiesHandler serves /capabilities/* only on the dedicated local-only listener.
func (h *Host) LocalCapabilitiesHandler() http.Handler { return h.localCapabilities }

func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		modelErr := h.modelService.Close(ctx)
		h.cancel()
		h.closeErr = errors.Join(modelErr, h.bridge.Close(ctx), h.controller.Close(), h.processes.Close())
	})
	return h.closeErr
}
