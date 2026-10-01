// Package openai composes the official HTTP forwarding provider with a private
// loopback MCP endpoint. The Runtime Host survives individual request failures.
package openai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	controlplane "github.com/openai/tunnel-client/pkg/controlplane/fx"
	"github.com/openai/tunnel-client/pkg/dispatcher"
	providerlog "github.com/openai/tunnel-client/pkg/log"
	"github.com/openai/tunnel-client/pkg/mcpclient"
	"github.com/openai/tunnel-client/pkg/metrics"
	"github.com/openai/tunnel-client/pkg/oauth"
	providerprocess "github.com/openai/tunnel-client/pkg/process"
	"github.com/openai/tunnel-client/pkg/runtimeconfig"
	"github.com/openai/tunnel-client/pkg/tlsconfig"
	"github.com/openai/tunnel-client/pkg/types"
	"go.uber.org/fx"
)

type Config struct {
	TunnelID string
	APIKey   string
}

func Run(ctx context.Context, server *mcp.Server, cfg Config) error {
	return run(ctx, server, cfg, runtimeconfig.DefaultControlPlaneBaseURL)
}

func run(ctx context.Context, server *mcp.Server, cfg Config, controlPlaneURL string) error {
	if err := runtimeconfig.ValidateTunnelID(cfg.TunnelID); err != nil {
		return err
	}
	if err := runtimeconfig.ValidateControlPlaneAPIKey(cfg.APIKey); err != nil {
		return err
	}
	baseURL, err := url.Parse(controlPlaneURL)
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "https" && baseURL.Scheme != "http") {
		return errors.New("invalid OpenAI control-plane URL")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	token := hex.EncodeToString(secret)
	httpServer, err := transport.NewHTTP(server, transport.HTTPConfig{Listen: "127.0.0.1:0", BearerToken: token, InternalToken: true})
	if err != nil {
		return err
	}
	endpoint, _ := url.Parse("http://" + httpServer.Address() + "/mcp")
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	httpDone := make(chan error, 1)
	go func() { httpDone <- httpServer.Run(runContext) }()
	client := newProvider(forwardingConfig(cfg, baseURL, endpoint, token))
	startContext, stopStart := context.WithTimeout(runContext, 15*time.Second)
	err = client.Start(startContext)
	stopStart()
	if err != nil {
		cancel()
		stopContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return errors.Join(err, client.Stop(stopContext), <-httpDone)
	}
	var result error
	select {
	case <-ctx.Done():
	case signal := <-client.Wait():
		// Fx and the CLI both observe OS shutdown signals. An orderly OS stop
		// must not depend on which signal receiver's goroutine ran first.
		if signal.ExitCode != 0 {
			result = errors.New("OpenAI tunnel provider stopped unexpectedly")
		}
	case result = <-httpDone:
		cancel()
		stopContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return errors.Join(result, client.Stop(stopContext))
	}
	cancel()
	stopContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return errors.Join(result, client.Stop(stopContext), <-httpDone)
}

// Compose maintained public provider modules without optional admin listeners,
// Harpoon, companion processes, or automatic OAuth discovery.
func newProvider(cfg *runtimeconfig.Config) *fx.App {
	return fx.New(
		fx.Supply(&cfg.ControlPlane, &cfg.Logging, &cfg.Process, &cfg.MCP),
		fx.Provide(func() *tlsconfig.Bundle { return nil }, oauth.NewDiscoveryState),
		providerlog.Module, metrics.MetricModule, mcpclient.Module,
		controlplane.Module, dispatcher.Module, providerprocess.Module, fx.NopLogger,
	)
}

func forwardingConfig(cfg Config, baseURL, endpoint *url.URL, token string) *runtimeconfig.Config {
	return &runtimeconfig.Config{
		ControlPlane: runtimeconfig.ControlPlaneConfig{
			BaseURL: baseURL, TunnelID: types.TunnelID(strings.TrimSpace(cfg.TunnelID)), APIKey: cfg.APIKey,
			MaxInFlightRequests: 20, PollChannels: []types.Channel{types.DefaultChannel}, PollChannelsConfigured: true,
		},
		Logging: runtimeconfig.LoggingConfig{Level: slog.LevelInfo, Format: runtimeconfig.LogFormatStructText},
		MCP: runtimeconfig.MCPConfig{
			ServerURL: endpoint, TransportKind: runtimeconfig.MCPTransportHTTPStreamable,
			ConnectionMaxTTL: 10 * time.Minute, MaxConcurrentRequests: 10,
			ExtraHeaders:    map[string]string{transport.InternalTokenHeader: token},
			ChannelBindings: []runtimeconfig.MCPChannelBinding{{Channel: types.DefaultChannel, TransportKind: runtimeconfig.MCPTransportHTTPStreamable, ServerURL: endpoint}},
		},
	}
}
