// Package openai connects an MCP server to OpenAI Secure MCP Tunnel.
package openai

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
)

type Config struct {
	TunnelID string
	APIKey   string
}

func Run(ctx context.Context, server *mcp.Server, cfg Config) error {
	if cfg.TunnelID == "" {
		return errors.New("OpenAI tunnel ID is required")
	}
	if cfg.APIKey == "" {
		return errors.New("OpenAI tunnel API key is required")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverTransport, tunnelTransport := mcp.NewInMemoryTransports()
	client, err := tunnelclient.New(tunnelclient.Config{TunnelID: cfg.TunnelID, APIKey: cfg.APIKey}, tunnelTransport)
	if err != nil {
		return err
	}
	serverErrors := make(chan error, 1)
	tunnelErrors := make(chan error, 1)
	go func() { serverErrors <- server.Run(ctx, serverTransport) }()
	go func() { tunnelErrors <- client.Run(ctx) }()
	select {
	case err := <-serverErrors:
		return err
	case err := <-tunnelErrors:
		return err
	case <-ctx.Done():
		return nil
	}
}
