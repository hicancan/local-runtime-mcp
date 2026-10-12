// Package connection composes one runtime with independently owned entrances.
package connection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
	cloudflaretunnel "github.com/hicancan/local-runtime-mcp/internal/tunnel/cloudflare"
	openaitunnel "github.com/hicancan/local-runtime-mcp/internal/tunnel/openai"
	"github.com/openai/tunnel-client/pkg/runtimeconfig"
)

type Options struct {
	Transport string
	Tunnels   []string
}

func (o Options) Validate() error {
	if o.Transport != "stdio" && o.Transport != "http" {
		return errors.New("--transport must be stdio or http")
	}
	if o.Transport == "stdio" && len(o.Tunnels) != 0 {
		return errors.New("tunnels require --transport http")
	}
	seen := make(map[string]bool)
	for _, name := range o.Tunnels {
		if name != "openai" && name != "cloudflare" {
			return fmt.Errorf("unknown tunnel %q; choose openai or cloudflare", name)
		}
		if seen[name] {
			return fmt.Errorf("tunnel %q was selected more than once", name)
		}
		seen[name] = true
	}
	return nil
}

func (o Options) has(name string) bool {
	for _, value := range o.Tunnels {
		if value == name {
			return true
		}
	}
	return false
}

type component struct {
	name string
	run  func(context.Context) error
}

// Run creates the resource owner once; credentials never implicitly enable an entrance.
func Run(ctx context.Context, cfg *config.Config, options Options, stdin io.Reader, stdout, stderr io.Writer) (result error) {
	if err := options.Validate(); err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("runtime configuration is required")
	}
	if options.has("openai") {
		if err := runtimeconfig.ValidateTunnelID(cfg.OpenAI.TunnelID); err != nil {
			return err
		}
		if err := runtimeconfig.ValidateControlPlaneAPIKey(cfg.OpenAI.APIKey); err != nil {
			return err
		}
	}
	if options.has("cloudflare") {
		if cfg.HTTP.PublicHost == "" || cfg.Cloudflare.TunnelToken == "" {
			return errors.New("Cloudflare requires http.public_host and cloudflare.tunnel_token")
		}
		if _, err := cloudflaretunnel.FindBinary(cfg.Cloudflare.Binary); err != nil {
			return err
		}
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result = errors.Join(result, host.Close(shutdown))
	}()
	if options.Transport == "stdio" {
		return normalizeCancellation(transport.ServeStdio(ctx, host.Server(), stdin, stdout))
	}
	log := &lockedWriter{writer: stderr}
	var components []component
	// OpenAI owns its private HTTP hop. Direct HTTP and Cloudflare use the
	// public-auth listener. Every entrance uses the same host.Server().
	if len(options.Tunnels) == 0 || options.has("cloudflare") {
		httpConfig, err := publicHTTPConfig(cfg)
		if err != nil {
			return err
		}
		server, err := transport.NewHTTP(host.Server(), httpConfig)
		if err != nil {
			return err
		}
		fmt.Fprintf(log, "[http] Streamable HTTP listening on http://%s/mcp (auth=%s)\n", server.Address(), cfg.HTTP.Auth)
		components = append(components, component{"http", server.Run})
	}
	if options.has("openai") {
		components = append(components, component{"openai", func(ctx context.Context) error {
			return openaitunnel.Run(ctx, host.Server(), openaitunnel.Config{TunnelID: cfg.OpenAI.TunnelID, APIKey: cfg.OpenAI.APIKey})
		}})
	}
	if options.has("cloudflare") {
		components = append(components, component{"cloudflare", func(ctx context.Context) error {
			return cloudflaretunnel.Run(ctx, cloudflaretunnel.Config{
				Binary: cfg.Cloudflare.Binary, Token: cfg.Cloudflare.TunnelToken,
				Stdout: log, Stderr: log,
			})
		}})
	}
	return runComponents(ctx, components, log)
}

func publicHTTPConfig(cfg *config.Config) (transport.HTTPConfig, error) {
	result := transport.HTTPConfig{Listen: cfg.HTTP.Listen, BearerToken: cfg.HTTP.BearerToken, PublicHost: cfg.HTTP.PublicHost}
	switch cfg.HTTP.Auth {
	case "", "bearer":
	case "oauth":
		if cfg.HTTP.PublicHost == "" {
			return result, errors.New("OAuth requires http.public_host")
		}
		oauthConfig := cfg.HTTP.OAuth
		oauthConfig.Issuer = "https://" + cfg.HTTP.PublicHost
		result.OAuth = &oauthConfig
	default:
		return result, errors.New("http.auth must be bearer or oauth")
	}
	return result, nil
}

// Each component gets the root lifetime, not a fail-fast group context. One
// provider ending does not cancel its peers or the shared machine resources.
func runComponents(ctx context.Context, components []component, log io.Writer) error {
	if len(components) == 0 {
		return errors.New("no connection entrances selected")
	}
	type outcome struct {
		name string
		err  error
	}
	done := make(chan outcome, len(components))
	for _, value := range components {
		go func(value component) { done <- outcome{value.name, normalizeCancellation(value.run(ctx))} }(value)
	}
	var failures []error
	for remaining := len(components); remaining > 0; {
		value := <-done
		remaining--
		if value.err != nil {
			failure := fmt.Errorf("%s: %w", value.name, value.err)
			failures = append(failures, failure)
			fmt.Fprintf(log, "[%s] entrance stopped with error: %v; %d entrances remain\n", value.name, value.err, remaining)
		} else if ctx.Err() == nil {
			fmt.Fprintf(log, "[%s] entrance stopped; %d entrances remain\n", value.name, remaining)
		}
	}
	return errors.Join(failures...)
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func normalizeCancellation(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		for _, child := range joined.Unwrap() {
			if cleaned := normalizeCancellation(child); cleaned != nil {
				remaining = append(remaining, cleaned)
			}
		}
		return errors.Join(remaining...)
	}
	if errors.Is(err, context.Canceled) || strings.HasPrefix(err.Error(), "server is closing:") {
		return nil
	}
	return err
}
