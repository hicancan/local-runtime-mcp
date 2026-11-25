package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/connection"
	"github.com/hicancan/local-runtime-mcp/internal/transport"
)

func uiCmd(ctx context.Context, args []string, stdout, stderr io.Writer, executable, configPath string) error {
	flags := flagSet("lrmcp ui", stderr)
	noOpen := flags.Bool("no-open", false, "print the one-time Studio launch URL without opening a browser")
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
	if _, err := connection.EnsureDaemon(ctx, executable, cfg, overrides); err != nil {
		return err
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodPost, "/admin/ui/bootstrap", nil, &resp); err != nil {
		return err
	}
	if resp.URL == "" {
		return errors.New("daemon returned an empty studio URL")
	}
	opened := false
	if !*noOpen {
		if err := openBrowserURL(resp.URL); err == nil {
			opened = true
		}
	}
	out := map[string]any{
		"opened":    opened,
		"studio":    "http://" + cfg.Local.Listen + "/ui/",
		"expires_s": 60,
	}
	if *noOpen || !opened {
		out["bootstrap_url"] = resp.URL
	}
	return writeJSON(stdout, out)
}

func openBrowserURL(target string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	case "darwin":
		return exec.Command("open", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

func auditCmd(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) error {
	if len(args) < 1 {
		return errors.New("audit requires list, events, show, search, tail, or export")
	}
	switch args[0] {
	case "list", "sessions":
		flags := flagSet("lrmcp audit list", stderr)
		limit := flags.Int("limit", 40, "maximum sessions to return")
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
		var resp struct {
			Sessions []audit.Session `json:"sessions"`
		}
		path := fmt.Sprintf("/admin/audit/sessions?limit=%d", *limit)
		if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodGet, path, nil, &resp); err != nil {
			return err
		}
		return writeJSON(stdout, resp)

	case "events":
		flags := flagSet("lrmcp audit events", stderr)
		sessionID := flags.String("session", "", "filter by session ID")
		agentID := flags.String("agent", "", "filter by agent ID")
		wire := flags.String("wire", "", "filter by wire (local, cloudflare, openai, http, web)")
		limit := flags.Int("limit", 100, "maximum events to return")
		compact := flags.Bool("compact", false, "omit bulky args_json and result_json payloads")
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
		q := url.Values{}
		q.Set("limit", fmt.Sprintf("%d", *limit))
		if *sessionID != "" {
			q.Set("session_id", *sessionID)
		}
		if *agentID != "" {
			q.Set("agent_id", *agentID)
		}
		if *wire != "" {
			q.Set("wire", *wire)
		}
		if *compact {
			q.Set("compact", "1")
		}
		var resp struct {
			Events []audit.Event `json:"events"`
		}
		if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodGet, "/admin/audit/events?"+q.Encode(), nil, &resp); err != nil {
			return err
		}
		return writeJSON(stdout, resp)

	case "show":
		flags := flagSet("lrmcp audit show", stderr)
		localFlags := addLocalFlags(flags)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("audit show requires one integer event ID")
		}
		id, err := strconv.ParseInt(flags.Arg(0), 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid event ID %q", flags.Arg(0))
		}
		var overrides config.Overrides
		localFlags.apply(&overrides)
		cfg, err := config.Resolve(configPath, overrides)
		if err != nil {
			return err
		}
		var resp struct {
			Event    audit.Event `json:"event"`
			BlobPath string      `json:"blob_path,omitempty"`
		}
		if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodGet, fmt.Sprintf("/admin/audit/events/%d", id), nil, &resp); err != nil {
			return err
		}
		return writeJSON(stdout, resp)

	case "search":
		flags := flagSet("lrmcp audit search", stderr)
		limit := flags.Int("limit", 40, "maximum matches to return")
		compact := flags.Bool("compact", false, "omit bulky args_json and result_json payloads")
		localFlags := addLocalFlags(flags)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		query := strings.TrimSpace(strings.Join(flags.Args(), " "))
		if query == "" {
			return errors.New("audit search requires a query string")
		}
		var overrides config.Overrides
		localFlags.apply(&overrides)
		cfg, err := config.Resolve(configPath, overrides)
		if err != nil {
			return err
		}
		q := url.Values{}
		q.Set("q", query)
		q.Set("limit", fmt.Sprintf("%d", *limit))
		if *compact {
			q.Set("compact", "1")
		}
		var resp struct {
			Query  string        `json:"query"`
			Events []audit.Event `json:"events"`
		}
		if err := transport.AdminRequest(ctx, cfg.Local.Listen, cfg.Local.Token, http.MethodGet, "/admin/audit/search?"+q.Encode(), nil, &resp); err != nil {
			return err
		}
		return writeJSON(stdout, resp)

	case "tail":
		flags := flagSet("lrmcp audit tail", stderr)
		agentID := flags.String("agent", "", "only show events for this agent ID")
		sessionID := flags.String("session", "", "only show events for this session ID")
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
		resp, err := transport.AdminStream(ctx, cfg.Local.Listen, cfg.Local.Token, "/admin/audit/stream")
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 2<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var pkt audit.LivePacket
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &pkt) != nil || pkt.Event == nil {
				continue
			}
			ev := pkt.Event
			if *agentID != "" && ev.AgentID != *agentID {
				continue
			}
			if *sessionID != "" && ev.SessionID != *sessionID {
				continue
			}
			ts := time.UnixMilli(ev.CreatedAt).Format("15:04:05")
			fmt.Fprintf(stdout, "[%s] [%s/%s] %-8s %-20s %s\n", ts, ev.Wire, ev.AgentID, ev.Status, ev.ToolName, ev.Summary)
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil

	case "export":
		flags := flagSet("lrmcp audit export", stderr)
		sessionID := flags.String("session", "", "export only one session ID")
		outputPath := flags.String("output", "", "write JSONL to file instead of stdout")
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
		path := "/admin/audit/export"
		if *sessionID != "" {
			path += "?session_id=" + url.QueryEscape(*sessionID)
		}
		resp, err := transport.AdminStream(ctx, cfg.Local.Listen, cfg.Local.Token, path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		target := stdout
		if *outputPath != "" {
			f, err := os.Create(*outputPath)
			if err != nil {
				return err
			}
			defer f.Close()
			target = f
		}
		_, err = io.Copy(target, resp.Body)
		return err

	default:
		return errors.New("audit requires list, events, show, search, tail, or export")
	}
}

func setupRemoteToken(args []string, stdout, stderr io.Writer, configPath string) error {
	flags := flagSet("lrmcp setup remote-token", stderr)
	id := flags.String("id", "", "named remote agent/client ID (lowercase letters, digits, - or _)")
	readOnly := flags.Bool("read-only", false, "restrict this token to read-only tools")
	var tools stringList
	flags.Var(&tools, "tool", "optional tool allowlist entry (repeatable)")
	show := flags.Bool("show", false, "show existing token credentials without changing configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if !config.ValidClientID(*id) {
		return errors.New("setup remote-token requires a valid lowercase --id, such as astra-1")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	index := -1
	for i, rt := range cfg.Grants.Remote.Tokens {
		if rt.ID == *id {
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
			cfg.Grants.Remote.Tokens = append(cfg.Grants.Remote.Tokens, config.RemoteToken{ID: *id, Token: tok})
			index = len(cfg.Grants.Remote.Tokens) - 1
		}
		rt := &cfg.Grants.Remote.Tokens[index]
		if rt.Token == "" {
			tok, err := secret()
			if err != nil {
				return err
			}
			rt.Token = tok
		}
		rt.ReadOnly = *readOnly
		if len(tools) > 0 {
			sorted := append([]string(nil), tools...)
			slices.Sort(sorted)
			rt.Tools = slices.Compact(sorted)
		}
		if _, err := config.Save(configPath, cfg); err != nil {
			return err
		}
	}
	if index < 0 {
		return errors.New("remote token is not configured; run lrmcp setup remote-token --id NAME first")
	}
	rt := cfg.Grants.Remote.Tokens[index]
	endpoint := "http://" + cfg.HTTP.Listen + "/mcp"
	if cfg.HTTP.PublicHost != "" {
		endpoint = "https://" + cfg.HTTP.PublicHost + "/mcp"
	}
	return writeJSON(stdout, struct {
		ID       string   `json:"id"`
		Token    string   `json:"token"`
		Endpoint string   `json:"endpoint"`
		ReadOnly bool     `json:"read_only,omitempty"`
		Tools    []string `json:"tools,omitempty"`
		NextStep string   `json:"next_step"`
	}{
		ID:       rt.ID,
		Token:    rt.Token,
		Endpoint: endpoint,
		ReadOnly: rt.ReadOnly,
		Tools:    rt.Tools,
		NextStep: "Send `Authorization: Bearer <token>` (and optional `X-LRMCP-Agent: <sub-agent>`) to " + endpoint + ". Reattach the http/cloudflare wire if already running.",
	})
}
