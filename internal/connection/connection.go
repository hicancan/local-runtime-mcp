// Package connection composes the runtime with one selected MCP connection mode.
package connection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
	cloudflaretunnel "github.com/hicancan/local-runtime-mcp/internal/tunnel/cloudflare"
	openaitunnel "github.com/hicancan/local-runtime-mcp/internal/tunnel/openai"
)

// ServeStdio serves the runtime over the MCP stdio transport.
func ServeStdio(ctx context.Context, cfg *config.Config, stdin io.Reader, stdout io.Writer) error {
	return withHost(ctx, cfg, func(host *runtimehost.Host) error {
		return transport.ServeStdio(ctx, host.Server(), stdin, stdout)
	})
}

// ServeHTTP serves the runtime over loopback MCP Streamable HTTP.
func ServeHTTP(ctx context.Context, cfg *config.Config, stderr io.Writer) error {
	if cfg.HTTP.BearerToken == "" {
		return errors.New("HTTP bearer_token is required in local-runtime-mcp.yaml, environment, or --token")
	}
	return withHost(ctx, cfg, func(host *runtimehost.Host) error {
		server, err := newHTTP(host, cfg)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Local Runtime MCP serving Streamable HTTP on http://%s/mcp\n", server.Address())
		return server.Run(ctx)
	})
}

// TunnelOpenAI connects the runtime to an OpenAI Secure MCP Tunnel.
func TunnelOpenAI(ctx context.Context, cfg *config.Config) error {
	if cfg.OpenAI.TunnelID == "" || cfg.OpenAI.APIKey == "" {
		return errors.New("OpenAI tunnel_id and api_key are required in local-runtime-mcp.yaml, environment, or command-line flags")
	}
	return withHost(ctx, cfg, func(host *runtimehost.Host) error {
		return openaitunnel.Run(ctx, host.Server(), openaitunnel.Config{
			TunnelID: cfg.OpenAI.TunnelID,
			APIKey:   cfg.OpenAI.APIKey,
		})
	})
}

// TunnelCloudflare serves Streamable HTTP and connects cloudflared to a
// remotely managed Cloudflare Tunnel.
func TunnelCloudflare(ctx context.Context, cfg *config.Config, stderr io.Writer) error {
	if cfg.HTTP.PublicHost == "" || cfg.HTTP.BearerToken == "" || cfg.Cloudflare.TunnelToken == "" {
		return errors.New("Cloudflare tunnel requires http.public_host, http.bearer_token, and cloudflare.tunnel_token")
	}
	return withHost(ctx, cfg, func(host *runtimehost.Host) error {
		server, err := newHTTP(host, cfg)
		if err != nil {
			return err
		}

		tunnelContext, cancel := context.WithCancel(ctx)
		defer cancel()
		httpErrors := make(chan error, 1)
		tunnelErrors := make(chan error, 1)
		go func() { httpErrors <- server.Run(tunnelContext) }()
		go func() {
			tunnelErrors <- cloudflaretunnel.Run(tunnelContext, cloudflaretunnel.Config{
				Binary: cfg.Cloudflare.Binary,
				Token:  cfg.Cloudflare.TunnelToken,
				SensitiveEnvironmentNames: []string{
					config.EnvBrowserToken,
					config.EnvOpenAIAPIKey,
					config.EnvHTTPBearerToken,
					config.EnvCloudflareTunnelToken,
				},
				Stdout: stderr,
				Stderr: stderr,
			})
		}()
		fmt.Fprintf(stderr, "Local Runtime MCP serving https://%s/mcp through Cloudflare Tunnel\n", cfg.HTTP.PublicHost)
		select {
		case err := <-httpErrors:
			return err
		case err := <-tunnelErrors:
			return err
		case <-tunnelContext.Done():
			return nil
		}
	})
}

func newHTTP(host *runtimehost.Host, cfg *config.Config) (*transport.HTTPServer, error) {
	return transport.NewHTTP(host.Server(), transport.HTTPConfig{
		Listen:      cfg.HTTP.Listen,
		BearerToken: cfg.HTTP.BearerToken,
		PublicHost:  cfg.HTTP.PublicHost,
	})
}

func withHost(ctx context.Context, cfg *config.Config, run func(*runtimehost.Host) error) error {
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	return normalizeCancellation(run(host))
}

func normalizeCancellation(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || strings.HasPrefix(err.Error(), "server is closing:") {
		return nil
	}
	return err
}
