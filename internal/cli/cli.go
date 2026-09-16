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
	"strings"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	cloudflareexposure "github.com/hicancan/local-runtime-mcp/internal/exposure/cloudflare"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/runtimehost"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
)

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return stdio(ctx, args, stdin, stdout, stderr)
	}
	switch args[0] {
	case "connect":
		return connect(ctx, args[1:], stderr)
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "expose":
		return expose(ctx, args[1:], stderr)
	case "browser-setup":
		return browserSetup(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		_, err := fmt.Fprintln(stdout, "lrmcp", mcpserver.Version)
		return err
	case "help", "--help", "-h":
		usage(stdout)
		return nil
	}
	if strings.HasPrefix(args[0], "-") {
		return stdio(ctx, args, stdin, stdout, stderr)
	}
	return fmt.Errorf("unknown command %q; run lrmcp help", args[0])
}

func stdio(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flagSet("lrmcp", stderr)
	configPath := flags.String("config", "", "configuration file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	cfg, err := config.Load(*configPath)
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

func connect(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "openai" {
		return errors.New("connect requires the provider name openai")
	}
	flags := flagSet("connect openai", stderr)
	configPath := flags.String("config", "", "configuration file path")
	tunnelID := flags.String("tunnel-id", os.Getenv("OPENAI_TUNNEL_ID"), "OpenAI tunnel ID")
	apiKeyEnv := flags.String("api-key-env", "OPENAI_TUNNEL_API_KEY", "environment variable containing the OpenAI tunnel API key")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *tunnelID == "" {
		return errors.New("OpenAI tunnel ID is required via --tunnel-id or OPENAI_TUNNEL_ID")
	}
	apiKey := os.Getenv(*apiKeyEnv)
	if apiKey == "" {
		return fmt.Errorf("OpenAI tunnel API key environment variable %s is empty", *apiKeyEnv)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	return normalizeCancellation(transport.ServeOpenAI(ctx, host.Server(), transport.OpenAIConfig{TunnelID: *tunnelID, APIKey: apiKey}))
}

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "http" {
		return errors.New("serve requires the transport name http")
	}
	flags := flagSet("serve http", stderr)
	configPath := flags.String("config", "", "configuration file path")
	listen := flags.String("listen", transport.DefaultHTTPListen, "loopback Streamable HTTP listen address")
	hostname := flags.String("hostname", os.Getenv("LRMCP_PUBLIC_HOST"), "optional public hostname accepted from a trusted reverse proxy")
	tokenFile := flags.String("token-file", os.Getenv("LRMCP_HTTP_TOKEN_FILE"), "file containing the HTTP bearer token")
	tokenEnv := flags.String("token-env", "LRMCP_HTTP_TOKEN", "environment variable containing the HTTP bearer token")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	token, err := loadSecret(*tokenFile, *tokenEnv)
	if err != nil {
		return err
	}
	return runHTTP(ctx, *configPath, *listen, *hostname, token, stderr)
}

func expose(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "cloudflare" {
		return errors.New("expose requires the provider name cloudflare")
	}
	flags := flagSet("expose cloudflare", stderr)
	configPath := flags.String("config", "", "configuration file path")
	listen := flags.String("listen", transport.DefaultHTTPListen, "loopback Streamable HTTP listen address")
	hostname := flags.String("hostname", os.Getenv("LRMCP_PUBLIC_HOST"), "public hostname routed by Cloudflare Tunnel")
	httpTokenFile := flags.String("http-token-file", os.Getenv("LRMCP_HTTP_TOKEN_FILE"), "file containing the HTTP bearer token")
	httpTokenEnv := flags.String("http-token-env", "LRMCP_HTTP_TOKEN", "environment variable containing the HTTP bearer token")
	cloudflared := flags.String("cloudflared", "", "cloudflared binary path; defaults to a sibling binary or PATH")
	tunnelTokenFile := flags.String("tunnel-token-file", os.Getenv("TUNNEL_TOKEN_FILE"), "file containing the Cloudflare tunnel token")
	tunnelTokenEnv := flags.String("tunnel-token-env", "TUNNEL_TOKEN", "environment variable containing the Cloudflare tunnel token")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *hostname == "" {
		return errors.New("Cloudflare exposure requires --hostname or LRMCP_PUBLIC_HOST")
	}
	httpToken, err := loadSecret(*httpTokenFile, *httpTokenEnv)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	httpServer, err := transport.NewHTTP(host.Server(), transport.HTTPConfig{
		Listen: *listen, BearerToken: httpToken, PublicHost: *hostname,
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
			Binary:                    *cloudflared,
			TokenFile:                 *tunnelTokenFile,
			Token:                     os.Getenv(*tunnelTokenEnv),
			SensitiveEnvironmentNames: []string{*httpTokenEnv, *tunnelTokenEnv},
			Stdout:                    stderr,
			Stderr:                    stderr,
		})
	}()
	fmt.Fprintf(stderr, "Local Runtime MCP exposing https://%s/mcp from %s\n", *hostname, httpServer.Address())
	select {
	case err := <-httpErrors:
		return normalizeCancellation(err)
	case err := <-cloudflareErrors:
		return normalizeCancellation(err)
	case <-ctx.Done():
		return nil
	}
}

func runHTTP(ctx context.Context, configPath, listen, hostname, token string, stderr io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	host, err := runtimehost.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	httpServer, err := transport.NewHTTP(host.Server(), transport.HTTPConfig{Listen: listen, BearerToken: token, PublicHost: hostname})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Local Runtime MCP serving Streamable HTTP on http://%s/mcp\n", httpServer.Address())
	return normalizeCancellation(httpServer.Run(ctx))
}

func browserSetup(args []string, stdout, stderr io.Writer) error {
	flags := flagSet("browser-setup", stderr)
	configPath := flags.String("config", "", "configuration file path")
	defaultDirectory, err := browser.DefaultExtensionDirectory()
	if err != nil {
		return err
	}
	directory := flags.String("directory", defaultDirectory, "extension output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	cfg, err := config.Load(*configPath)
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
	resolvedConfig, err := config.Save(*configPath, cfg)
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

func loadSecret(path, environment string) (string, error) {
	value := os.Getenv(environment)
	if path != "" && value != "" {
		return "", fmt.Errorf("provide secret file or environment variable %s, not both", environment)
	}
	if path == "" {
		return strings.TrimSpace(value), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func usage(writer io.Writer) {
	fmt.Fprint(writer, `Local Runtime MCP (lrmcp)

Usage:
  lrmcp [--config PATH]                  MCP over stdio
  lrmcp connect openai [flags]           MCP over OpenAI Tunnel
  lrmcp serve http [flags]               MCP over loopback Streamable HTTP
  lrmcp expose cloudflare [flags]        Streamable HTTP through Cloudflare Tunnel
  lrmcp browser-setup [flags]            Configure and extract the Chromium extension
  lrmcp version
  lrmcp help

OpenAI flags:
  --config PATH
  --tunnel-id ID                         Or OPENAI_TUNNEL_ID
  --api-key-env NAME                     Defaults to OPENAI_TUNNEL_API_KEY

HTTP flags:
  --config PATH
  --listen ADDRESS                       Defaults to 127.0.0.1:9316
  --hostname HOST                        Optional trusted reverse-proxy hostname
  --token-file PATH                      Or LRMCP_HTTP_TOKEN_FILE
  --token-env NAME                       Defaults to LRMCP_HTTP_TOKEN

Cloudflare flags:
  --config PATH
  --listen ADDRESS                       Defaults to 127.0.0.1:9316
  --hostname HOST                        Or LRMCP_PUBLIC_HOST
  --http-token-file PATH                 Or LRMCP_HTTP_TOKEN_FILE
  --http-token-env NAME                  Defaults to LRMCP_HTTP_TOKEN
  --cloudflared PATH                     Defaults to sibling companion or PATH
  --tunnel-token-file PATH               Or TUNNEL_TOKEN_FILE
  --tunnel-token-env NAME                Defaults to TUNNEL_TOKEN

Source: https://github.com/hicancan/local-runtime-mcp
License: GNU AGPL v3.0 only
`)
}
