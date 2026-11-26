// Package connection composes one unified user-session daemon with
// independently hot-pluggable remote wires and stateless stdio thin proxies.
package connection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
	"github.com/hicancan/local-runtime-mcp/internal/ui"
)

type Options struct {
	Transport       string   // "daemon" or "stdio"
	HTTP            bool     // attach public loopback HTTP (9316) on daemon startup
	Tunnels         []string // optional initial remote wires ("openai", "cloudflare")
	Executable      string   // current executable for detached daemon auto-spawn
	Agent           string   // optional logical client/agent label for stdio activity attribution
	Overrides       config.Overrides
	AutoSpawnDaemon bool // in stdio mode with local.token, spawn detached daemon instead of hosting in-process
}

func (o Options) Validate() error {
	if o.Transport != "daemon" && o.Transport != "stdio" {
		return errors.New("transport must be daemon or stdio")
	}
	if o.Transport != "daemon" && (len(o.Tunnels) != 0 || o.HTTP) {
		return errors.New("tunnels and public HTTP require daemon mode")
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

// EnsureDaemon verifies the unified local daemon is listening on cfg.Local.Listen
// and, if not, spawns `lrmcp daemon` as a detached user-session background process
// (forwarding any non-empty CLI overrides via child environment variables so secrets
// never appear on the process command line) and waits until it becomes healthy.
func EnsureDaemon(ctx context.Context, executable string, cfg *config.Config, overrides config.Overrides) (bool, error) {
	if cfg == nil || cfg.Local.Token == "" {
		return false, errors.New("local.token is required to manage the unified daemon; run lrmcp setup local")
	}
	if err := config.ValidateLocal(cfg.Local, len(cfg.Grants.Clients) > 0); err != nil {
		return false, err
	}
	if transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
		return false, nil
	}
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return false, fmt.Errorf("find executable: %w", err)
		}
	}
	cmd := exec.Command(executable, "daemon")
	cmd.Env = daemonEnv(os.Environ(), overrides)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start detached daemon: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
	return false, fmt.Errorf("detached daemon did not become ready on http://%s within 8s", cfg.Local.Listen)
}

func daemonEnv(base []string, overrides config.Overrides) []string {
	env := append([]string(nil), base...)
	set := func(key, value string) {
		if value == "" {
			return
		}
		prefix := key + "="
		for i, entry := range env {
			if strings.HasPrefix(strings.ToUpper(entry), strings.ToUpper(prefix)) {
				env[i] = prefix + value
				return
			}
		}
		env = append(env, prefix+value)
	}
	set(config.EnvBrowserListen, overrides.BrowserListen)
	set(config.EnvBrowserToken, overrides.BrowserToken)
	set(config.EnvLocalListen, overrides.LocalListen)
	set(config.EnvLocalToken, overrides.LocalToken)
	set(config.EnvOpenAITunnelID, overrides.OpenAITunnelID)
	set(config.EnvOpenAIAPIKey, overrides.OpenAIAPIKey)
	set(config.EnvHTTPListen, overrides.HTTPListen)
	set(config.EnvHTTPPublicHost, overrides.HTTPPublicHost)
	set(config.EnvHTTPBearerToken, overrides.HTTPBearerToken)
	set(config.EnvHTTPAuth, overrides.HTTPAuth)
	set(config.EnvOAuthOwnerToken, overrides.OAuthOwnerToken)
	set(config.EnvCloudflareTunnelToken, overrides.CloudflareTunnelToken)
	set(config.EnvCloudflared, overrides.Cloudflared)
	return env
}

// Run either starts the unified user-session daemon (hosting 9317, 9315, and
// hot-pluggable remote wires) or attaches a stdio MCP client as a stateless thin
// proxy (auto-spawning the detached daemon when local.token is configured).
func Run(ctx context.Context, cfg *config.Config, options Options, stdin io.Reader, stdout, stderr io.Writer) (result error) {
	if err := options.Validate(); err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("runtime configuration is required")
	}

	hasLocal := cfg.Local.Token != "" || len(cfg.Grants.Clients) > 0
	if options.Transport == "daemon" && !hasLocal {
		return errors.New("daemon requires local.token or grants.clients; run lrmcp setup local")
	}

	// Stdio mode with local.token configured: always attach cleanly as a stateless
	// thin proxy to the unified daemon (auto-spawning a detached daemon if needed).
	if options.Transport == "stdio" && cfg.Local.Token != "" {
		if err := config.ValidateLocal(cfg.Local, len(cfg.Grants.Clients) > 0); err != nil {
			return err
		}
		if transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
			fmt.Fprintf(stderr, "[stdio] attached to running local runtime at http://%s/mcp\n", cfg.Local.Listen)
			return normalizeCancellation(transport.ProxyStdioToLocalWithAgent(ctx, cfg.Local.Listen, cfg.Local.Token, options.Agent, stdin, stdout))
		}
		if options.AutoSpawnDaemon {
			spawned, err := EnsureDaemon(ctx, options.Executable, cfg, options.Overrides)
			if err != nil {
				return err
			}
			if spawned {
				fmt.Fprintf(stderr, "[stdio] started detached local runtime daemon and attached at http://%s/mcp\n", cfg.Local.Listen)
			} else {
				fmt.Fprintf(stderr, "[stdio] attached to running local runtime at http://%s/mcp\n", cfg.Local.Listen)
			}
			return normalizeCancellation(transport.ProxyStdioToLocalWithAgent(ctx, cfg.Local.Listen, cfg.Local.Token, options.Agent, stdin, stdout))
		}
	}

	daemonCtx, stopDaemon := context.WithCancel(ctx)
	defer stopDaemon()

	host, err := runtimehost.Start(daemonCtx, cfg)
	if err != nil {
		return err
	}
	closeHost := func() error {
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return host.Close(shutdown)
	}
	closed := false
	defer func() {
		if !closed {
			result = errors.Join(result, closeHost())
		}
	}()

	log := &lockedWriter{writer: stderr}

	if options.Transport == "stdio" {
		if hasLocal {
			var localSrv *transport.LocalServer
			ingress := newIngressManager(daemonCtx, cfg, host, log, func() int { return localSrv.AttachedClients() })
			host.SetTunnelController(ingress)
			kick := func(id string) bool {
				a := localSrv.KickAttached(id)
				b := host.KickSession(id)
				return a || b
			}
			var studio *ui.Studio
			if cfg.Local.Token != "" {
				var studioErr error
				studio, studioErr = newStudio(func() string { return localSrv.Address() }, host, ingress, kick)
				if studioErr != nil {
					ingress.Close()
					return studioErr
				}
				host.SetUIBootstrap(func() (string, error) { return studio.MintBootstrapURL(localSrv.Address()) })
			}
			var localErr error
			localSrv, localErr = newLocalHTTP(host, cfg, newAdminHandler(func() string { return localSrv.Address() }, host, ingress, studio, kick, stopDaemon), studio)
			if localErr != nil {
				ingress.Close()
				if cfg.Local.Token != "" && transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
					closed = true
					_ = closeHost()
					fmt.Fprintf(stderr, "[stdio] attached to running local runtime at http://%s/mcp\n", cfg.Local.Listen)
					return normalizeCancellation(transport.ProxyStdioToLocalWithAgent(ctx, cfg.Local.Listen, cfg.Local.Token, options.Agent, stdin, stdout))
				}
				return localErr
			}
			localDone := make(chan error, 1)
			go func() { localDone <- normalizeCancellation(localSrv.Run(daemonCtx)) }()
			stdioCtx := daemonCtx
			if cleanAgent := audit.SanitizeID(options.Agent); cleanAgent != "" {
				stdioCtx = audit.WithCallMeta(stdioCtx, audit.CallMeta{Wire: "local", AgentID: cleanAgent})
			}
			stdioErr := normalizeCancellation(transport.ServeStdio(stdioCtx, host.LocalServer(), stdin, stdout))
			stopDaemon()
			ingress.Close()
			return errors.Join(stdioErr, <-localDone)
		}
		// Zero-config isolated stdio mode (no local.token configured).
		stdioCtx := daemonCtx
		if cleanAgent := audit.SanitizeID(options.Agent); cleanAgent != "" {
			stdioCtx = audit.WithCallMeta(stdioCtx, audit.CallMeta{Wire: "local", AgentID: cleanAgent})
		}
		return normalizeCancellation(transport.ServeStdio(stdioCtx, host.LocalServer(), stdin, stdout))
	}

	// Unified daemon mode: always hosts 9317 (local plane + admin + /ui studio) and 9315
	// (browser bridge), with remote wires (openai / cloudflare / http) hot-pluggable on demand.
	var localSrv *transport.LocalServer
	ingress := newIngressManager(daemonCtx, cfg, host, log, func() int { return localSrv.AttachedClients() })
	defer ingress.Close()
	host.SetTunnelController(ingress)

	kick := func(id string) bool {
		a := localSrv.KickAttached(id)
		b := host.KickSession(id)
		return a || b
	}
	var studio *ui.Studio
	if cfg.Local.Token != "" {
		studio, err = newStudio(func() string { return localSrv.Address() }, host, ingress, kick)
		if err != nil {
			return err
		}
		host.SetUIBootstrap(func() (string, error) { return studio.MintBootstrapURL(localSrv.Address()) })
	}

	localSrv, err = newLocalHTTP(host, cfg, newAdminHandler(func() string { return localSrv.Address() }, host, ingress, studio, kick, stopDaemon), studio)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "[daemon] local plane listening on http://%s (mcp=%t, studio=%t, clients=%d)\n", localSrv.Address(), cfg.Local.Token != "", studio != nil, len(cfg.Grants.Clients))

	if options.HTTP {
		if _, err := ingress.Up("http", 0); err != nil {
			return err
		}
	}
	for _, tunnel := range options.Tunnels {
		if _, err := ingress.Up(tunnel, 0); err != nil {
			return err
		}
	}

	err = normalizeCancellation(localSrv.Run(daemonCtx))
	ingress.Close()
	return err
}

func newLocalHTTP(host *runtimehost.Host, cfg *config.Config, admin, studio http.Handler) (*transport.LocalServer, error) {
	address := cfg.Local.Listen
	if address == "" {
		address = config.DefaultLocal
	}
	return transport.NewLocalHTTP(host.LocalServer(), transport.LocalConfig{
		Listen:         address,
		Token:          cfg.Local.Token,
		Version:        mcpserver.Version,
		Capabilities:   host.LocalCapabilitiesHandler(),
		Admin:          admin,
		UI:             studio,
		OnSessionClose: host.CleanupLocalSession,
	})
}

func publicHTTPConfig(cfg *config.Config) (transport.HTTPConfig, error) {
	tokens := make([]transport.RemoteTokenAuth, 0, len(cfg.Grants.Remote.Tokens))
	for _, rt := range cfg.Grants.Remote.Tokens {
		tokens = append(tokens, transport.RemoteTokenAuth{
			ID:       rt.ID,
			Token:    rt.Token,
			ReadOnly: rt.ReadOnly,
			Tools:    append([]string(nil), rt.Tools...),
		})
	}
	wire := "http"
	if cfg.Cloudflare.TunnelToken != "" && cfg.HTTP.PublicHost != "" {
		wire = "cloudflare"
	}
	result := transport.HTTPConfig{
		Listen:       cfg.HTTP.Listen,
		BearerToken:  cfg.HTTP.BearerToken,
		PublicHost:   cfg.HTTP.PublicHost,
		RemoteTokens: tokens,
		Wire:         wire,
	}
	switch cfg.HTTP.Auth {
	case "", "bearer":
	case "oauth":
		if cfg.HTTP.PublicHost == "" {
			return result, errors.New("OAuth requires http.public_host")
		}
		oauthConfig := cfg.HTTP.OAuth
		oauthConfig.Issuer = "https://" + cfg.HTTP.PublicHost
		if cfg.Path != "" {
			oauthConfig.StorePath = filepath.Join(filepath.Dir(cfg.Path), "lrmcp-oauth-grants.json")
		}
		result.OAuth = &oauthConfig
	default:
		return result, errors.New("http.auth must be bearer or oauth")
	}
	return result, nil
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
