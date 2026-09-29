// Package openai connects an MCP server to OpenAI Secure MCP Tunnel.
package openai

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
)

const reconnectDelay = 250 * time.Millisecond

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

	return run(ctx, server, func(transport mcp.Transport) (tunnelRunner, error) {
		return tunnelclient.New(tunnelclient.Config{TunnelID: cfg.TunnelID, APIKey: cfg.APIKey}, transport)
	}, reconnectDelay)
}

type tunnelRunner interface {
	Run(context.Context) error
}

type clientFactory func(mcp.Transport) (tunnelRunner, error)

type generationExit int

const (
	generationStopped generationExit = iota
	generationServerEnded
	generationTunnelEnded
)

// run keeps the provider process alive when a request deadline closes the
// embedded in-memory MCP session. A fresh transport pair is required because
// the Go SDK implements that transport with net.Pipe, which cannot be reopened.
func run(ctx context.Context, server *mcp.Server, newClient clientFactory, retryDelay time.Duration) error {
	for {
		exit, err := runGeneration(ctx, server, newClient)
		switch exit {
		case generationStopped:
			return nil
		case generationTunnelEnded:
			return err
		case generationServerEnded:
			if !waitForReconnect(ctx, retryDelay) {
				return nil
			}
		}
	}
}

func runGeneration(ctx context.Context, server *mcp.Server, newClient clientFactory) (generationExit, error) {
	serverTransport, tunnelTransport := mcp.NewInMemoryTransports()
	client, err := newClient(tunnelTransport)
	if err != nil {
		return generationTunnelEnded, err
	}

	generationContext, cancel := context.WithCancel(ctx)
	serverErrors := make(chan error, 1)
	tunnelErrors := make(chan error, 1)
	go func() { serverErrors <- server.Run(generationContext, serverTransport) }()
	go func() { tunnelErrors <- client.Run(generationContext) }()

	select {
	case <-ctx.Done():
		cancel()
		<-serverErrors
		<-tunnelErrors
		return generationStopped, nil
	case err := <-serverErrors:
		cancel()
		<-tunnelErrors
		return generationServerEnded, err
	case err := <-tunnelErrors:
		cancel()
		<-serverErrors
		if ctx.Err() != nil {
			return generationStopped, nil
		}
		return generationTunnelEnded, err
	}
}

func waitForReconnect(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
