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

// Host owns the machine-local dependencies shared by every connection mode.
// Connections only determine how MCP messages reach this one runtime.
type Host struct {
	server             *mcp.Server
	bridge             *browser.Bridge
	controller         computer.Controller
	processes          *runtimeprocess.Manager
	models             http.Handler
	modelService       *modelapi.Service
	nativeCapabilities http.Handler
	cancel             context.CancelFunc
	closeOnce          sync.Once
	closeErr           error
}

func Start(ctx context.Context, cfg *config.Config, localApps bool) (*Host, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("runtime configuration is required")
	}
	if err := config.ValidateBrowser(cfg.Browser); err != nil {
		return nil, err
	}
	if err := validateWebConfig(cfg); err != nil {
		return nil, err
	}
	nativeGrant, err := nativeCapabilityGrant(cfg)
	if err != nil {
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
	bridge.SetWebHandler(webHandler(cfg.Capabilities.Web, registry, models))
	var nativeCapabilities http.Handler
	if nativeGrant != nil {
		nativeCapabilities = nativeCapabilityHandler(*nativeGrant, cfg.Capabilities.Native.BrowserID, registry, bridge.Status)
	}
	controller := computer.New(ctx)
	bridge.SetDesktopGate(controller.BeginDesktopOperation)
	processes := runtimeprocess.NewManager(ctx)
	return &Host{
		server:             mcpserver.New(mcpserver.Dependencies{Process: processes, Filesystem: filesystem.New(), Browser: bridge, Computer: controller, Capabilities: registry, LocalApps: localApps}),
		bridge:             bridge,
		controller:         controller,
		processes:          processes,
		models:             &modelapi.HTTP{Service: models, Key: cfg.Models.Key},
		modelService:       models,
		nativeCapabilities: nativeCapabilities,
		cancel:             cancel,
	}, nil
}

func (h *Host) Server() *mcp.Server { return h.server }

// ModelsHandler uses its own model-only key and is never the private MCP hop.
func (h *Host) ModelsHandler() http.Handler { return h.models }

// NativeCapabilitiesHandler is mounted only on the dedicated local listener.
func (h *Host) NativeCapabilitiesHandler() http.Handler { return h.nativeCapabilities }

func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		modelErr := h.modelService.Close(ctx)
		h.cancel()
		h.closeErr = errors.Join(modelErr, h.bridge.Close(ctx), h.controller.Close(), h.processes.Close())
	})
	return h.closeErr
}
