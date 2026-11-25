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
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/connection"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/hicancan/local-runtime-mcp/internal/oauth"
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
		return errors.New("command required; run lrmcp help")
	}
	switch args[0] {
	case "daemon":
		return daemonCmd(ctx, args[1:], stdin, stdout, stderr, executable, configPath)
	case "start":
		return startCmd(ctx, args[1:], stdout, stderr, executable, configPath)
	case "stop":
		return stopCmd(ctx, args[1:], stdout, stderr, configPath)
	case "status":
		return statusCmd(ctx, args[1:], stdout, stderr, configPath)
	case "tunnel":
		return tunnelCmd(ctx, args[1:], stdout, stderr, executable, configPath)
	case "grant":
		return grantCmd(ctx, args[1:], stdout, stderr, configPath)
	case "ui":
		return uiCmd(ctx, args[1:], stdout, stderr, executable, configPath)
	case "audit":
		return auditCmd(ctx, args[1:], stdout, stderr, configPath)
	case "serve":
		return serveCmd(ctx, args[1:], stdin, stdout, stderr, executable, configPath)
	case "setup":
		if len(args) < 2 {
			return errors.New("setup requires browser, local, client, oauth, or remote-token")
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
		case "remote-token":
			return setupRemoteToken(args[2:], stdout, stderr, configPath)
		default:
			return errors.New("setup requires browser, local, client, oauth, or remote-token")
		}
	case "version", "--version", "-v":
		if len(args) != 1 {
			return errors.New("version does not accept arguments")
		}
		_, err := fmt.Fprintln(stdout, "lrmcp", mcpserver.Version)
		return err
	case "help", "--help", "-h":
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

func daemonCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, executable, configPath string) error {
	flags := flagSet("lrmcp daemon", stderr)
	enableHTTP := flags.Bool("http", false, "also open public loopback HTTP listener on startup")
	var tunnels stringList
	flags.Var(&tunnels, "tunnel", "initial remote wire: openai or cloudflare; repeat to attach both")
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
	overrides := browserFlags.overrides()
	localFlags.apply(&overrides)
	httpFlags.apply(&overrides)
	overrides.OpenAITunnelID, overrides.OpenAIAPIKey = *openaiID, *openaiKey
	overrides.CloudflareTunnelToken, overrides.Cloudflared = *cloudflareToken, *cloudflared
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	options := connection.Options{
		Transport:  "daemon",
		HTTP:       *enableHTTP,
		Tunnels:    tunnels,
		Executable: executable,
		Overrides:  overrides,
	}
	return connection.Run(ctx, cfg, options, stdin, stdout, stderr)
}

func startCmd(ctx context.Context, args []string, stdout, stderr io.Writer, executable, configPath string) error {
	flags := flagSet("lrmcp start", stderr)
	enableHTTP := flags.Bool("http", false, "attach public loopback HTTP wire after ensuring daemon is running")
	var tunnels stringList
	flags.Var(&tunnels, "tunnel", "remote wire to attach: openai or cloudflare; may be repeated")
	ttl := flags.Duration("ttl", 0, "optional automatic expiry lease for attached wires (e.g. 2h)")
	browserFlags := addBrowserFlags(flags)
	localFlags := addLocalFlags(flags)
	httpFlags := addHTTPFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *ttl < 0 {
		return errors.New("--ttl cannot be negative")
	}
	overrides := browserFlags.overrides()
	localFlags.apply(&overrides)
	httpFlags.apply(&overrides)
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	spawned, err := connection.EnsureDaemon(ctx, executable, cfg, overrides)
	if err != nil {
		return err
	}
	ttlSeconds := int64(ttl.Seconds())
	if *enableHTTP {
		if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodPost, "/admin/tunnels/up", map[string]any{"name": "http", "ttl_seconds": ttlSeconds}, nil); err != nil {
			return err
		}
	}
	for _, name := range tunnels {
		if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodPost, "/admin/tunnels/up", map[string]any{"name": name, "ttl_seconds": ttlSeconds}, nil); err != nil {
			return err
		}
	}
	var st connection.DaemonStatus
	if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodGet, "/admin/status", nil, &st); err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"started": spawned, "daemon": st})
}

func stopCmd(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp stop", stderr)
	localFlags := addLocalFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	var overrides config.Overrides
	localFlags.apply(&overrides)
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	if !transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
		return writeJSON(stdout, map[string]any{"stopped": true, "was_running": false})
	}
	if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodPost, "/admin/shutdown", nil, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	return writeJSON(stdout, map[string]any{"stopped": true, "was_running": true})
}

func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp status", stderr)
	localFlags := addLocalFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	var overrides config.Overrides
	localFlags.apply(&overrides)
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	if cfg.Local.Token == "" {
		return errors.New("local.token is not configured; run lrmcp setup local")
	}
	if !transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
		return writeJSON(stdout, map[string]any{"running": false, "local_listen": cfg.Local.Listen})
	}
	var st connection.DaemonStatus
	if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodGet, "/admin/status", nil, &st); err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"running": true, "daemon": st})
}

func tunnelCmd(ctx context.Context, args []string, stdout, stderr io.Writer, executable, configPath string) error {
	if len(args) < 1 {
		return errors.New("tunnel requires up, down, or status")
	}
	switch args[0] {
	case "up":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("tunnel up requires a wire name: openai, cloudflare, or http")
		}
		name := args[1]
		flags := flagSet("lrmcp tunnel up", stderr)
		ttl := flags.Duration("ttl", 0, "optional automatic expiry lease (e.g. 2h, 30m)")
		localFlags := addLocalFlags(flags)
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", flags.Arg(0))
		}
		if *ttl < 0 {
			return errors.New("--ttl cannot be negative")
		}
		var overrides config.Overrides
		localFlags.apply(&overrides)
		cfg, err := config.Resolve(configPath, overrides)
		if err != nil {
			return err
		}
		if _, err := connection.EnsureDaemon(ctx, executable, cfg, overrides); err != nil {
			return err
		}
		var resp struct {
			Tunnels []runtimehost.TunnelStatus `json:"tunnels"`
		}
		err = transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodPost, "/admin/tunnels/up", map[string]any{
			"name":        name,
			"ttl_seconds": int64(ttl.Seconds()),
		}, &resp)
		if err != nil {
			return err
		}
		return writeJSON(stdout, resp)

	case "down":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("tunnel down requires a wire name: openai, cloudflare, http, or all")
		}
		name := args[1]
		flags := flagSet("lrmcp tunnel down", stderr)
		localFlags := addLocalFlags(flags)
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", flags.Arg(0))
		}
		var overrides config.Overrides
		localFlags.apply(&overrides)
		cfg, err := config.Resolve(configPath, overrides)
		if err != nil {
			return err
		}
		var resp struct {
			Tunnels []runtimehost.TunnelStatus `json:"tunnels"`
		}
		err = transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodPost, "/admin/tunnels/down", map[string]any{"name": name}, &resp)
		if err != nil {
			return err
		}
		return writeJSON(stdout, resp)

	case "status":
		flags := flagSet("lrmcp tunnel status", stderr)
		localFlags := addLocalFlags(flags)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", flags.Arg(0))
		}
		var overrides config.Overrides
		localFlags.apply(&overrides)
		cfg, err := config.Resolve(configPath, overrides)
		if err != nil {
			return err
		}
		var st connection.DaemonStatus
		if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodGet, "/admin/status", nil, &st); err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"tunnels": st.Tunnels, "remote_grants": st.RemoteGrants})

	default:
		return errors.New("tunnel requires up, down, or status")
	}
}

func grantCmd(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) error {
	if len(args) < 2 || args[0] != "remote" {
		return errors.New("grant requires: lrmcp grant remote <openai|http> [--read-only] [--tool NAME]")
	}
	target := args[1]
	if target != "openai" && target != "http" {
		return fmt.Errorf("unknown remote target %q; choose openai or http", target)
	}
	flags := flagSet("lrmcp grant remote", stderr)
	readOnly := flags.Bool("read-only", false, "restrict this remote wire to read-only tools only")
	var tools stringList
	flags.Var(&tools, "tool", "allowlisted tool name (repeatable; empty allows all non-excluded tools)")
	localFlags := addLocalFlags(flags)
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	var overrides config.Overrides
	localFlags.apply(&overrides)
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	grant := config.RemoteGrant{ReadOnly: *readOnly, Tools: tools}
	slices.Sort(grant.Tools)
	grant.Tools = slices.Compact(grant.Tools)

	if cfg.Local.Token != "" && transport.ProbeLocal(ctx, cfg.Local.Listen, cfg.Local.Token) {
		var resp struct {
			RemoteGrants config.RemoteGrants `json:"remote_grants"`
		}
		err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodPost, "/admin/remote-grants", map[string]any{
			"target": target,
			"grant":  grant,
		}, &resp)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"live": true, "remote_grants": resp.RemoteGrants})
	}

	stored, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if target == "openai" {
		stored.Grants.Remote.OpenAI = grant
	} else {
		stored.Grants.Remote.HTTP = grant
	}
	if _, err := config.Save(configPath, stored); err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"live": false, "remote_grants": stored.Grants.Remote})
}

func serveCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, executable, configPath string) error {
	flags := flagSet("lrmcp serve", stderr)
	protocol := flags.String("transport", "", "MCP transport: stdio (required)")
	agent := flags.String("agent", "", "optional logical client/agent label shown in the activity stream (or LOCAL_RUNTIME_MCP_AGENT_ID)")
	browserFlags := addBrowserFlags(flags)
	localFlags := addLocalFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *protocol != "stdio" {
		return errors.New("serve only accepts --transport stdio; use lrmcp daemon / lrmcp start / lrmcp tunnel up for background & remote wires")
	}
	overrides := browserFlags.overrides()
	localFlags.apply(&overrides)
	cfg, err := config.Resolve(configPath, overrides)
	if err != nil {
		return err
	}
	agentName := strings.TrimSpace(*agent)
	if agentName == "" {
		agentName = strings.TrimSpace(os.Getenv(config.EnvironmentPrefix + "AGENT_ID"))
	}
	options := connection.Options{
		Transport:       "stdio",
		Executable:      executable,
		Agent:           agentName,
		Overrides:       overrides,
		AutoSpawnDaemon: true,
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
		"Point local MCP clients at `lrmcp serve --transport stdio` (auto-starts the detached daemon) or run `lrmcp start` / `lrmcp tunnel up openai`."})
}

func setupClient(args []string, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp setup client", stderr)
	id := flags.String("id", "", "local client ID (lowercase letters, digits, - or _)")
	show := flags.Bool("show", false, "show existing client credentials locally without changing configuration")
	browserID := flags.String("browser-id", "", "optional browser profile ID bound to this client")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if !config.ValidClientID(*id) {
		return errors.New("setup client requires a valid lowercase --id, such as studev-win")
	}
	if *show && *browserID != "" {
		return errors.New("--show does not accept --browser-id")
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
		Capabilities map[string]string `json:"capabilities,omitempty"`
		BrowserID    string            `json:"browser_id,omitempty"`
		NextStep     string            `json:"next_step"`
	}{client.ID, client.Token, "http://" + cfg.Local.Listen, client.Capabilities, client.BrowserID,
		"Use this scoped token against http://" + cfg.Local.Listen + "/capabilities. Keep credentials private."})
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
		"Enter client_id and client_secret in the client. On the consent page, enter owner_token to approve access. Keep these credentials private."})
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
	return localFlagValues{flags.String("local-listen", "", "local-only loopback address (default 127.0.0.1:9317)"), flags.String("local-token", "", "local owner token for same-host MCP, stdio attach, and admin control")}
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
  lrmcp serve --transport stdio [--agent NAME]       Pure stdio MCP proxy (auto-starts detached daemon)
  lrmcp start [--http] [--tunnel openai|cloudflare] [--ttl 2h]
  lrmcp stop                                         Gracefully stop the running daemon
  lrmcp status                                       Inspect live daemon, browser profiles, and tunnels
  lrmcp daemon [--http] [--tunnel openai|cloudflare] Run unified daemon in foreground
  lrmcp tunnel up <openai|cloudflare|http> [--ttl 2h]
  lrmcp tunnel down <openai|cloudflare|http|all>
  lrmcp tunnel status
  lrmcp grant remote <openai|http> [--read-only] [--tool NAME]
  lrmcp ui [--no-open]                               Open local Sovereign Activity & Audit Studio
  lrmcp audit list [--limit N]                       List audited sessions
  lrmcp audit events [--session ID] [--compact]      List recorded tool calls & notes
  lrmcp audit show <event_id>                        Inspect one full audit event by ID
  lrmcp audit search <query> [--compact] [--limit N] FTS5 full-text search across audit history
  lrmcp audit tail [--agent ID] [--session ID]       Stream live events in real time
  lrmcp audit export [--session ID] [--output FILE]  Export audit stream as JSONL
  lrmcp setup browser [flags]
  lrmcp setup local [flags]
  lrmcp setup client --id NAME [--browser-id ID]
  lrmcp setup client --show --id NAME
  lrmcp setup oauth --client-name gemini --redirect-uri URI [flags]
  lrmcp setup oauth --show [--client-name NAME]
  lrmcp setup remote-token --id NAME [--read-only] [--tool NAME]
  lrmcp setup remote-token --show --id NAME
  lrmcp version
  lrmcp help

Configuration:
  local-runtime-mcp.yaml beside lrmcp
  precedence: command line > LOCAL_RUNTIME_MCP_* environment > YAML > defaults

Unified architecture:
  One detached user-session daemon owns the OS runtime (127.0.0.1:9317) and the
  bidirectional browser extension bridge (127.0.0.1:9315). All local MCP clients
  attach concurrently as stateless thin proxies; closing any IDE never stops the
  daemon. Remote wires (openai, cloudflare, http) plug and unplug live without
  restarting the daemon and obey per-wire grants.remote boundaries.

Project: https://github.com/hicancan/local-runtime-mcp
License: GNU AGPL v3.0 only
`)
}
