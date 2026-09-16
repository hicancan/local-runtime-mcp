package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	cloudflareexposure "github.com/hicancan/local-runtime-mcp/internal/exposure/cloudflare"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
)

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	return run(ctx, args, stdin, stdout, stderr, executable)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, executable string) error {
	configPath, err := config.PathForExecutable(executable)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return stdio(ctx, args, stdin, stdout, stderr, configPath)
	}
	switch args[0] {
	case "connect":
		return connect(ctx, args[1:], stderr, configPath)
	case "serve":
		return serve(ctx, args[1:], stderr, configPath)
	case "expose":
		return expose(ctx, args[1:], stderr, configPath)
	case "browser-setup":
		return browserSetup(args[1:], stdout, stderr, executable, configPath)
	case "version", "--version", "-v":
		_, err := fmt.Fprintln(stdout, "lrmcp", mcpserver.Version)
		return err
	case "help", "--help", "-h":
		usage(stdout)
		return nil
	}
	if strings.HasPrefix(args[0], "-") {
		return stdio(ctx, args, stdin, stdout, stderr, configPath)
	}
	return fmt.Errorf("unknown command %q; run lrmcp help", args[0])
}

func stdio(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp", stderr)
	browserFlags := addBrowserFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	cfg, err := config.Resolve(configPath, browserFlags.overrides())
	if err != nil {
		return err
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	return normalizeCancellation(transport.ServeStdio(ctx, host.Server(), stdin, stdout))
}

func connect(ctx context.Context, args []string, stderr io.Writer, configPath string) error {
	if len(args) == 0 || args[0] != "openai" {
		return errors.New("connect requires the provider name openai")
	}
	flags := flagSet("connect openai", stderr)
	browserFlags := addBrowserFlags(flags)
	tunnelID := flags.String("tunnel-id", "", "OpenAI tunnel ID")
	apiKey := flags.String("api-key", "", "OpenAI tunnel API key")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	overrides := browserFlags.overrides()
	overrides.OpenAITunnelID = *tunnelID
	overrides.OpenAIAPIKey = *apiKey
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	if cfg.OpenAI.TunnelID == "" || cfg.OpenAI.APIKey == "" {
		return errors.New("OpenAI tunnel_id and api_key are required in local-runtime-mcp.yaml, environment, or command-line flags")
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	return normalizeCancellation(transport.ServeOpenAI(ctx, host.Server(), transport.OpenAIConfig{
		TunnelID: cfg.OpenAI.TunnelID,
		APIKey:   cfg.OpenAI.APIKey,
	}))
}

func serve(ctx context.Context, args []string, stderr io.Writer, configPath string) error {
	if len(args) == 0 || args[0] != "http" {
		return errors.New("serve requires the transport name http")
	}
	flags := flagSet("serve http", stderr)
	browserFlags := addBrowserFlags(flags)
	httpFlags := addHTTPFlags(flags)
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	overrides := browserFlags.overrides()
	httpFlags.apply(&overrides)
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	return runHTTP(ctx, cfg, stderr)
}

func expose(ctx context.Context, args []string, stderr io.Writer, configPath string) error {
	if len(args) == 0 || args[0] != "cloudflare" {
		return errors.New("expose requires the provider name cloudflare")
	}
	flags := flagSet("expose cloudflare", stderr)
	browserFlags := addBrowserFlags(flags)
	httpFlags := addHTTPFlags(flags)
	tunnelToken := flags.String("tunnel-token", "", "Cloudflare managed-tunnel token")
	cloudflared := flags.String("cloudflared", "", "cloudflared binary path; defaults to the sibling companion or PATH")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	overrides := browserFlags.overrides()
	httpFlags.apply(&overrides)
	overrides.CloudflareTunnelToken = *tunnelToken
	overrides.Cloudflared = *cloudflared
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	if cfg.HTTP.PublicHost == "" || cfg.HTTP.BearerToken == "" || cfg.Cloudflare.TunnelToken == "" {
		return errors.New("Cloudflare exposure requires http.public_host, http.bearer_token, and cloudflare.tunnel_token")
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	httpServer, err := transport.NewHTTP(host.Server(), transport.HTTPConfig{
		Listen: cfg.HTTP.Listen, BearerToken: cfg.HTTP.BearerToken, PublicHost: cfg.HTTP.PublicHost,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpErrors := make(chan error, 1)
	cloudflareErrors := make(chan error, 1)
	go func() { httpErrors <- httpServer.Run(ctx) }()
	go func() {
		cloudflareErrors <- cloudflareexposure.Run(ctx, cloudflareexposure.Config{
			Binary: cfg.Cloudflare.Binary,
			Token:  cfg.Cloudflare.TunnelToken,
			SensitiveEnvironmentNames: []string{
				config.EnvBrowserToken, config.EnvOpenAIAPIKey,
				config.EnvHTTPBearerToken, config.EnvCloudflareTunnelToken,
			},
			Stdout: stderr,
			Stderr: stderr,
		})
	}()
	fmt.Fprintf(stderr, "Local Runtime MCP exposing https://%s/mcp from %s\n", cfg.HTTP.PublicHost, httpServer.Address())
	select {
	case err := <-httpErrors:
		return normalizeCancellation(err)
	case err := <-cloudflareErrors:
		return normalizeCancellation(err)
	case <-ctx.Done():
		return nil
	}
}

func runHTTP(ctx context.Context, cfg *config.Config, stderr io.Writer) error {
	if cfg.HTTP.BearerToken == "" {
		return errors.New("HTTP bearer_token is required in local-runtime-mcp.yaml, environment, or --token")
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	httpServer, err := transport.NewHTTP(host.Server(), transport.HTTPConfig{
		Listen: cfg.HTTP.Listen, BearerToken: cfg.HTTP.BearerToken, PublicHost: cfg.HTTP.PublicHost,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Local Runtime MCP serving Streamable HTTP on http://%s/mcp\n", httpServer.Address())
	return normalizeCancellation(httpServer.Run(ctx))
}

func browserSetup(args []string, stdout, stderr io.Writer, executable, configPath string) error {
	flags := flagSet("browser-setup", stderr)
	browserFlags := addBrowserFlags(flags)
	defaultDirectory := filepath.Join(filepath.Dir(executable), "browser-extension")
	directory := flags.String("directory", defaultDirectory, "extension output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	stored, err := config.Load(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Resolve(configPath, browserFlags.overrides())
	if err != nil {
		return err
	}
	if cfg.Browser.Token == "" {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return err
		}
		cfg.Browser.Token = hex.EncodeToString(value)
	}
	stored.Browser = cfg.Browser
	resolvedConfig, err := config.Save(configPath, stored)
	if err != nil {
		return err
	}
	installed, err := browser.InstallExtension(*directory)
	if err != nil {
		return err
	}
	if err := browser.ConfigureExtension(installed, cfg.Browser.Listen, cfg.Browser.Token); err != nil {
		return err
	}
	return writeJSON(stdout, struct {
		Directory string `json:"directory"`
		Config    string `json:"config"`
		Address   string `json:"address"`
		NextStep  string `json:"next_step"`
	}{
		Directory: installed,
		Config:    resolvedConfig,
		Address:   cfg.Browser.Listen,
		NextStep:  "Open edge://extensions or chrome://extensions, enable Developer mode, choose Load unpacked, and select the extension directory.",
	})
}

type browserFlagValues struct {
	listen *string
	token  *string
}

func addBrowserFlags(flags *flag.FlagSet) browserFlagValues {
	return browserFlagValues{
		listen: flags.String("browser-listen", "", "authenticated browser bridge loopback address"),
		token:  flags.String("browser-token", "", "browser bridge token"),
	}
}

func (values browserFlagValues) overrides() config.Overrides {
	return config.Overrides{BrowserListen: *values.listen, BrowserToken: *values.token}
}

type httpFlagValues struct {
	listen   *string
	hostname *string
	token    *string
}

func addHTTPFlags(flags *flag.FlagSet) httpFlagValues {
	return httpFlagValues{
		listen:   flags.String("listen", "", "loopback Streamable HTTP listen address"),
		hostname: flags.String("hostname", "", "public hostname accepted from the reverse proxy"),
		token:    flags.String("token", "", "HTTP bearer token"),
	}
}

func (values httpFlagValues) apply(overrides *config.Overrides) {
	overrides.HTTPListen = *values.listen
	overrides.HTTPPublicHost = *values.hostname
	overrides.HTTPBearerToken = *values.token
}

func flagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func normalizeCancellation(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || strings.HasPrefix(err.Error(), "server is closing:") {
		return nil
	}
	return err
}

func usage(writer io.Writer) {
	fmt.Fprint(writer, `Local Runtime MCP (lrmcp)

Usage:
  lrmcp [flags]                         MCP over stdio
  lrmcp connect openai [flags]          MCP over OpenAI Tunnel
  lrmcp serve http [flags]              MCP over loopback Streamable HTTP
  lrmcp expose cloudflare [flags]       Streamable HTTP through Cloudflare Tunnel
  lrmcp browser-setup [flags]           Configure and extract the Chromium extension
  lrmcp version
  lrmcp help

Configuration:
  local-runtime-mcp.yaml beside lrmcp
  precedence: command line > LOCAL_RUNTIME_MCP_* environment > YAML > defaults

Common browser flags:
  --browser-listen ADDRESS
  --browser-token TOKEN

OpenAI flags:
  --tunnel-id ID
  --api-key KEY

HTTP flags:
  --listen ADDRESS
  --hostname HOST
  --token TOKEN

Cloudflare flags:
  --listen ADDRESS
  --hostname HOST
  --token TOKEN
  --tunnel-token TOKEN
  --cloudflared PATH

Source: https://github.com/hicancan/local-runtime-mcp
License: GNU AGPL v3.0 only
`)
}
