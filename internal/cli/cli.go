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

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/connection"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
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
		return errors.New("command required; run lrmcp help")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stdin, stdout, stderr, configPath)
	case "tunnel":
		return tunnel(ctx, args[1:], stderr, configPath)
	case "setup":
		return setup(args[1:], stdout, stderr, executable, configPath)
	case "version":
		_, err := fmt.Fprintln(stdout, "lrmcp", mcpserver.Version)
		return err
	case "help":
		usage(stdout)
		return nil
	}
	return fmt.Errorf("unknown command %q; run lrmcp help", args[0])
}

func serve(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, configPath string) error {
	if len(args) == 0 {
		return errors.New("serve requires the transport name stdio or http")
	}
	switch args[0] {
	case "stdio":
		flags := flagSet("lrmcp serve stdio", stderr)
		browserFlags := addBrowserFlags(flags)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", flags.Arg(0))
		}
		cfg, err := config.Resolve(configPath, browserFlags.overrides())
		if err != nil {
			return err
		}
		return connection.ServeStdio(ctx, cfg, stdin, stdout)
	case "http":
		return serveHTTP(ctx, args[1:], stderr, configPath)
	default:
		return fmt.Errorf("unknown serve transport %q; choose stdio or http", args[0])
	}
}

func serveHTTP(ctx context.Context, args []string, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp serve http", stderr)
	browserFlags := addBrowserFlags(flags)
	httpFlags := addHTTPFlags(flags)
	if err := flags.Parse(args); err != nil {
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
	return connection.ServeHTTP(ctx, cfg, stderr)
}

func tunnel(ctx context.Context, args []string, stderr io.Writer, configPath string) error {
	if len(args) == 0 {
		return errors.New("tunnel requires the provider name openai or cloudflare")
	}
	switch args[0] {
	case "openai":
		return tunnelOpenAI(ctx, args[1:], stderr, configPath)
	case "cloudflare":
		return tunnelCloudflare(ctx, args[1:], stderr, configPath)
	default:
		return fmt.Errorf("unknown tunnel provider %q; choose openai or cloudflare", args[0])
	}
}

func tunnelOpenAI(ctx context.Context, args []string, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp tunnel openai", stderr)
	browserFlags := addBrowserFlags(flags)
	tunnelID := flags.String("tunnel-id", "", "OpenAI tunnel ID")
	apiKey := flags.String("api-key", "", "OpenAI tunnel API key")
	if err := flags.Parse(args); err != nil {
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
	return connection.TunnelOpenAI(ctx, cfg)
}

func tunnelCloudflare(ctx context.Context, args []string, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp tunnel cloudflare", stderr)
	browserFlags := addBrowserFlags(flags)
	httpFlags := addHTTPFlags(flags)
	tunnelToken := flags.String("tunnel-token", "", "Cloudflare managed-tunnel token")
	cloudflared := flags.String("cloudflared", "", "cloudflared binary path; defaults to the sibling companion or PATH")
	if err := flags.Parse(args); err != nil {
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
	return connection.TunnelCloudflare(ctx, cfg, stderr)
}

func setup(args []string, stdout, stderr io.Writer, executable, configPath string) error {
	if len(args) == 0 || args[0] != "browser" {
		return errors.New("setup requires the component name browser")
	}
	return setupBrowser(args[1:], stdout, stderr, executable, configPath)
}

func setupBrowser(args []string, stdout, stderr io.Writer, executable, configPath string) error {
	flags := flagSet("lrmcp setup browser", stderr)
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

func usage(writer io.Writer) {
	fmt.Fprint(writer, `Local Runtime MCP (lrmcp)

Usage:
  lrmcp serve stdio [flags]             MCP over stdio
  lrmcp serve http [flags]              MCP over loopback Streamable HTTP
  lrmcp tunnel openai [flags]           MCP through OpenAI Secure MCP Tunnel
  lrmcp tunnel cloudflare [flags]       Streamable HTTP through Cloudflare Tunnel
  lrmcp setup browser [flags]           Configure and extract the Chromium extension
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
