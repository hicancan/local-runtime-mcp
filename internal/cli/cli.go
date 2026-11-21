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
	"slices"
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
			return errors.New("setup requires browser, local, client, or oauth")
		}
		switch args[1] {
		case "browser":
			return setupBrowser(args[2:], stdout, stderr, executable, configPath)
		case "local":
			return setupLocal(args[2:], stdout, stderr, configPath)
		case "client":
			return setupClient(args[2:], stdout, stderr, configPath)
		case "oauth":
			return setupOAuth(args[2:], stdout, stderr, configPath)
		default:
			return errors.New("setup requires browser, local, client, or oauth")
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
	protocol := flags.String("transport", "", "MCP transport: stdio, local, or http (required)")
	var tunnels stringList
	flags.Var(&tunnels, "tunnel", "HTTP tunnel provider: openai or cloudflare; repeat to enable both")
	browserFlags := addBrowserFlags(flags)
	localFlags := addLocalFlags(flags)
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
	localFlags.apply(&overrides)
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

func setupLocal(args []string, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp setup local", stderr)
	localFlags := addLocalFlags(flags)
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
	overrides := config.Overrides{}
	localFlags.apply(&overrides)
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	if cfg.Local.Token == "" {
		cfg.Local.Token, err = secret()
		if err != nil {
			return err
		}
	}
	if err := config.ValidateLocal(cfg.Local, false); err != nil {
		return err
	}
	stored.Local = cfg.Local
	resolvedConfig, err := config.Save(configPath, stored)
	if err != nil {
		return err
	}
	return writeJSON(stdout, struct {
		Config   string `json:"config"`
		Address  string `json:"address"`
		NextStep string `json:"next_step"`
	}{resolvedConfig, cfg.Local.Listen,
		"Run lrmcp serve --transport local (or --transport http ...) once, and point local MCP clients at lrmcp serve --transport stdio to share one runtime."})
}

func setupClient(args []string, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp setup client", stderr)
	id := flags.String("id", "", "local client ID (lowercase letters, digits, - or _)")
	show := flags.Bool("show", false, "show existing client credentials locally without changing configuration")
	browserID := flags.String("browser-id", "", "optional browser profile ID bound to this client")
	var models stringList
	flags.Var(&models, "model", "registered model ID granted to this client; may be repeated")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if !config.ValidClientID(*id) {
		return errors.New("setup client requires a valid lowercase --id, such as studev-win or harness")
	}
	if *show && (len(models) != 0 || *browserID != "") {
		return errors.New("--show does not accept --model or --browser-id")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	index := -1
	for i, client := range cfg.Grants.Clients {
		if client.ID == *id {
			index = i
			break
		}
	}
	if !*show {
		if index < 0 {
			tok, err := secret()
			if err != nil {
				return err
			}
			cfg.Grants.Clients = append(cfg.Grants.Clients, config.ClientGrant{ID: *id, Token: tok})
			index = len(cfg.Grants.Clients) - 1
		}
		client := &cfg.Grants.Clients[index]
		if client.Token == "" {
			tok, err := secret()
			if err != nil {
				return err
			}
			client.Token = tok
		}
		for _, m := range models {
			if !slices.Contains(client.Models, m) {
				client.Models = append(client.Models, m)
			}
		}
		slices.Sort(client.Models)
		if *browserID != "" {
			client.BrowserID = *browserID
		}
		if _, err := config.Save(configPath, cfg); err != nil {
			return err
		}
	}
	if index < 0 {
		return errors.New("local client is not configured; run lrmcp setup client --id NAME first")
	}
	client := cfg.Grants.Clients[index]
	return writeJSON(stdout, struct {
		ClientID     string            `json:"client_id"`
		Token        string            `json:"token"`
		LocalURL     string            `json:"local_url"`
		Models       []string          `json:"models,omitempty"`
		Capabilities map[string]string `json:"capabilities,omitempty"`
		BrowserID    string            `json:"browser_id,omitempty"`
		NextStep     string            `json:"next_step"`
	}{client.ID, client.Token, "http://" + cfg.Local.Listen, client.Models, client.Capabilities, client.BrowserID,
		"Use this scoped token against http://" + cfg.Local.Listen + "/v1 and /capabilities. Keep credentials private."})
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

type localFlagValues struct{ listen, token *string }

func addLocalFlags(flags *flag.FlagSet) localFlagValues {
	return localFlagValues{flags.String("local-listen", "", "local-only loopback address (default 127.0.0.1:9317)"), flags.String("local-token", "", "local owner token for same-host MCP and stdio attach")}
}
func (v localFlagValues) apply(o *config.Overrides) {
	o.LocalListen = *v.listen
	o.LocalToken = *v.token
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
  lrmcp serve --transport local [flags]
  lrmcp serve --transport http [flags]
  lrmcp serve --transport http --tunnel openai [flags]
  lrmcp serve --transport http --tunnel cloudflare [flags]
  lrmcp serve --transport http --tunnel openai --tunnel cloudflare [flags]
  lrmcp setup browser [flags]
  lrmcp setup local [flags]
  lrmcp setup client --id NAME [--model ID] [--browser-id ID]
  lrmcp setup client --show --id NAME
  lrmcp setup oauth --client-name gemini --redirect-uri URI [flags]
  lrmcp setup oauth --show [--client-name NAME]
  lrmcp version
  lrmcp help

Configuration:
  local-runtime-mcp.yaml beside lrmcp
  precedence: command line > LOCAL_RUNTIME_MCP_* environment > YAML > defaults

Serve flags:
  --transport stdio|local|http        Required MCP transport
  --tunnel openai|cloudflare          Repeat with --transport http to attach both providers
  --browser-listen ADDRESS
  --browser-token TOKEN
  --local-listen ADDRESS              Local-only plane (default 127.0.0.1:9317; never tunneled)
  --local-token TOKEN                 Local owner token for /mcp and stdio thin-proxy attach
  --listen ADDRESS                    Public-auth HTTP listener (default 127.0.0.1:9316)
  --hostname HOSTNAME
  --token TOKEN                       Static public HTTP Bearer authentication
  --http-auth bearer|oauth
  --tunnel-id ID                      OpenAI provider
  --api-key KEY                       OpenAI provider
  --tunnel-token TOKEN                Cloudflare provider
  --cloudflared PATH                  Cloudflare companion

Local clients share one daemon on 127.0.0.1:9317 (/mcp, /v1, /capabilities).
Public HTTP (127.0.0.1:9316) serves remote /mcp and OAuth only, never /v1 or LocalApps.
OpenAI retains its independently authenticated private HTTP hop.

Project: https://github.com/hicancan/local-runtime-mcp
License: GNU AGPL v3.0 only
`)
}
