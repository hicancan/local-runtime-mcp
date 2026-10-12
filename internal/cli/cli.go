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
	"github.com/hicancan/local-runtime-mcp/internal/connection"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/oauth"
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
	case "setup":
		if len(args) < 2 {
			return errors.New("setup requires browser or oauth")
		}
		switch args[1] {
		case "browser":
			return setupBrowser(args[2:], stdout, stderr, executable, configPath)
		case "oauth":
			return setupOAuth(args[2:], stdout, stderr, configPath)
		default:
			return errors.New("setup requires browser or oauth")
		}
	case "version":
		if len(args) != 1 {
			return errors.New("version does not accept arguments")
		}
		_, err := fmt.Fprintln(stdout, "lrmcp", mcpserver.Version)
		return err
	case "help":
		usage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run lrmcp help", args[0])
	}
}

type stringList []string

func (v *stringList) String() string { return strings.Join(*v, ",") }
func (v *stringList) Set(value string) error {
	if value == "" {
		return errors.New("value cannot be empty")
	}
	*v = append(*v, value)
	return nil
}

func serve(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp serve", stderr)
	protocol := flags.String("transport", "", "MCP transport: stdio or http (required)")
	var tunnels stringList
	flags.Var(&tunnels, "tunnel", "HTTP tunnel provider: openai or cloudflare; repeat to enable both")
	browserFlags := addBrowserFlags(flags)
	httpFlags := addHTTPFlags(flags)
	openaiID := flags.String("tunnel-id", "", "OpenAI tunnel ID")
	openaiKey := flags.String("api-key", "", "OpenAI tunnel API key")
	cloudflareToken := flags.String("tunnel-token", "", "Cloudflare managed-tunnel token")
	cloudflared := flags.String("cloudflared", "", "cloudflared companion path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	options := connection.Options{Transport: *protocol, Tunnels: tunnels}
	if err := options.Validate(); err != nil {
		return err
	}
	overrides := browserFlags.overrides()
	httpFlags.apply(&overrides)
	overrides.OpenAITunnelID, overrides.OpenAIAPIKey = *openaiID, *openaiKey
	overrides.CloudflareTunnelToken, overrides.Cloudflared = *cloudflareToken, *cloudflared
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	return connection.Run(ctx, cfg, options, stdin, stdout, stderr)
}

func setupBrowser(args []string, stdout, stderr io.Writer, executable, configPath string) error {
	flags := flagSet("lrmcp setup browser", stderr)
	browserFlags := addBrowserFlags(flags)
	directory := flags.String("directory", filepath.Join(filepath.Dir(executable), "browser-extension"), "extension output directory")
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
		cfg.Browser.Token, err = secret()
		if err != nil {
			return err
		}
	}
	if err := config.ValidateBrowser(cfg.Browser); err != nil {
		return err
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
	}{installed, resolvedConfig, cfg.Browser.Listen,
		"Load or reload the extension directory at edge://extensions or chrome://extensions."})
}

func setupOAuth(args []string, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp setup oauth", stderr)
	name := flags.String("client-name", "gemini", "registered OAuth client ID")
	show := flags.Bool("show", false, "show existing credentials locally without changing configuration")
	var redirects stringList
	flags.Var(&redirects, "redirect-uri", "exact HTTPS callback copied from the client; may be repeated")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *show && len(redirects) != 0 {
		return errors.New("--show does not accept --redirect-uri")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	index := -1
	for i, client := range cfg.HTTP.OAuth.Clients {
		if client.ID == *name {
			index = i
			break
		}
	}
	if !*show {
		if len(redirects) == 0 {
			return errors.New("setup oauth requires --redirect-uri; use --show to view an existing client")
		}
		if index < 0 {
			value, err := secret()
			if err != nil {
				return err
			}
			cfg.HTTP.OAuth.Clients = append(cfg.HTTP.OAuth.Clients, oauth.Client{ID: *name, Secret: value})
			index = len(cfg.HTTP.OAuth.Clients) - 1
		}
		client := &cfg.HTTP.OAuth.Clients[index]
		for _, redirect := range redirects {
			found := false
			for _, existing := range client.RedirectURIs {
				found = found || existing == redirect
			}
			if !found {
				client.RedirectURIs = append(client.RedirectURIs, redirect)
			}
		}
		if cfg.HTTP.OAuth.OwnerToken == "" {
			cfg.HTTP.OAuth.OwnerToken, err = secret()
			if err != nil {
				return err
			}
		}
		cfg.HTTP.Auth = "oauth"
		if cfg.HTTP.PublicHost == "" {
			return errors.New("set http.public_host before registering an OAuth client")
		}
		validation := cfg.HTTP.OAuth
		validation.Issuer = "https://" + cfg.HTTP.PublicHost
		if _, err := oauth.New(validation); err != nil {
			return err
		}
		if _, err := config.Save(configPath, cfg); err != nil {
			return err
		}
	}
	if index < 0 {
		return errors.New("OAuth client is not configured; register its exact --redirect-uri first")
	}
	client := cfg.HTTP.OAuth.Clients[index]
	// Printing secrets is an explicit local setup operation, never a serve log.
	return writeJSON(stdout, struct {
		ServerURL    string   `json:"server_url"`
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		OwnerToken   string   `json:"owner_token"`
		RedirectURIs []string `json:"redirect_uris"`
		NextStep     string   `json:"next_step"`
	}{"https://" + cfg.HTTP.PublicHost + "/mcp", client.ID, client.Secret, cfg.HTTP.OAuth.OwnerToken, client.RedirectURIs,
		"Enter client_id and client_secret in the client. On the consent page, enter owner_token to approve full machine access. Keep these credentials private."})
}

func secret() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

type browserFlagValues struct{ listen, token *string }

func addBrowserFlags(flags *flag.FlagSet) browserFlagValues {
	return browserFlagValues{flags.String("browser-listen", "", "browser bridge loopback address"), flags.String("browser-token", "", "browser bridge token")}
}
func (v browserFlagValues) overrides() config.Overrides {
	return config.Overrides{BrowserListen: *v.listen, BrowserToken: *v.token}
}

type httpFlagValues struct{ listen, hostname, token, auth *string }

func addHTTPFlags(flags *flag.FlagSet) httpFlagValues {
	return httpFlagValues{flags.String("listen", "", "public-auth loopback HTTP listen address"), flags.String("hostname", "", "public MCP hostname"), flags.String("token", "", "static HTTP Bearer token"), flags.String("http-auth", "", "public HTTP authentication: bearer or oauth")}
}
func (v httpFlagValues) apply(o *config.Overrides) {
	o.HTTPListen = *v.listen
	o.HTTPPublicHost = *v.hostname
	o.HTTPBearerToken = *v.token
	o.HTTPAuth = *v.auth
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
  lrmcp serve --transport stdio [flags]
  lrmcp serve --transport http [flags]
  lrmcp serve --transport http --tunnel openai [flags]
  lrmcp serve --transport http --tunnel cloudflare [flags]
  lrmcp serve --transport http --tunnel openai --tunnel cloudflare [flags]
  lrmcp setup browser [flags]
  lrmcp setup oauth --client-name gemini --redirect-uri URI [flags]
  lrmcp setup oauth --show [--client-name NAME]
  lrmcp version
  lrmcp help

Configuration:
  local-runtime-mcp.yaml beside lrmcp
  precedence: command line > LOCAL_RUNTIME_MCP_* environment > YAML > defaults

Serve flags:
  --transport stdio|http              Required MCP transport
  --tunnel openai|cloudflare          Repeat to attach both providers to one Host
  --browser-listen ADDRESS
  --browser-token TOKEN
  --listen ADDRESS                   Public-auth HTTP listener
  --hostname HOSTNAME
  --token TOKEN                      Static Bearer authentication
  --http-auth bearer|oauth
  --tunnel-id ID                      OpenAI provider
  --api-key KEY                       OpenAI provider
  --tunnel-token TOKEN                Cloudflare provider
  --cloudflared PATH                  Cloudflare companion

OAuth setup stores client credentials and an owner approval key beside the executable.
OpenAI retains its independently authenticated private HTTP hop.

Project: https://github.com/hicancan/local-runtime-mcp
License: GNU AGPL v3.0 only
`)
}
