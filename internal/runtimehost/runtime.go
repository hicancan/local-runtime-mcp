package runtimehost

import (
	"context"
	"errors"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Host owns the machine-local dependencies shared by every connection mode.
// Connections only determine how MCP messages reach this one runtime.
type Host struct {
	server     *mcp.Server
	bridge     *browser.Bridge
	controller computer.Controller
}

func Start(ctx context.Context, cfg *config.Config) (*Host, error) {
	bridge, err := browser.Start(ctx, cfg.Browser)
	if err != nil {
		return nil, err
	}
	controller := computer.New(ctx)
	return &Host{
		server:     mcpserver.New(ctx, bridge, controller),
		bridge:     bridge,
		controller: controller,
	}, nil
}

func (h *Host) Server() *mcp.Server { return h.server }

func (h *Host) Close(ctx context.Context) error {
	return errors.Join(h.bridge.Close(ctx), h.controller.Close())
}
