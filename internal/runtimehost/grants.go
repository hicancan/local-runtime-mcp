package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// grantStore holds the owner's web and remote grants while the Host runs. The
// configuration file stays their one source: a change from the extension panel
// or admin API takes effect at once and is written back into that file, touching
// nothing else in it (environment overrides and secrets never leave memory).
type grantStore struct {
	mu             sync.RWMutex
	bySite         map[string]config.WebGrant
	remote         config.RemoteGrants
	path           string
	registry       *capability.Registry
	machine        *machineTools
	controller     computer.Controller
	audit          *audit.Store
	tunnels        TunnelController
	uiBootstrap    func() (string, error)
	onRemoteChange func(target string, grant config.RemoteGrant)
}

func newGrantStore(
	grants []config.WebGrant,
	remote config.RemoteGrants,
	path string,
	registry *capability.Registry,
	machine *machineTools,
	controller computer.Controller,
	auditStore *audit.Store,
	onRemoteChange func(target string, grant config.RemoteGrant),
) *grantStore {
	store := &grantStore{
		bySite:         map[string]config.WebGrant{},
		remote:         cloneRemoteGrants(remote),
		path:           path,
		registry:       registry,
		machine:        machine,
		controller:     controller,
		audit:          auditStore,
		onRemoteChange: onRemoteChange,
	}
	for _, g := range grants {
		store.bySite[g.Site] = g
	}
	return store
}

func cloneRemoteGrants(r config.RemoteGrants) config.RemoteGrants {
	tokens := make([]config.RemoteToken, len(r.Tokens))
	for i, tok := range r.Tokens {
		tokens[i] = config.RemoteToken{
			ID:       tok.ID,
			Token:    tok.Token,
			ReadOnly: tok.ReadOnly,
			Tools:    append([]string(nil), tok.Tools...),
		}
	}
	return config.RemoteGrants{
		OpenAI: config.RemoteGrant{ReadOnly: r.OpenAI.ReadOnly, Tools: append([]string(nil), r.OpenAI.Tools...)},
		HTTP:   config.RemoteGrant{ReadOnly: r.HTTP.ReadOnly, Tools: append([]string(nil), r.HTTP.Tools...)},
		Tokens: tokens,
	}
}

func (s *grantStore) setTunnels(ctrl TunnelController) {
	s.mu.Lock()
	s.tunnels = ctrl
	s.mu.Unlock()
}

func (s *grantStore) setUIBootstrap(fn func() (string, error)) {
	s.mu.Lock()
	s.uiBootstrap = fn
	s.mu.Unlock()
}

func (s *grantStore) remoteCopy() config.RemoteGrants {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRemoteGrants(s.remote)
}

// get finds the grant covering one page origin.
func (s *grantStore) get(origin string) (config.WebGrant, bool) {
	site, err := config.SiteOf(origin)
	if err != nil {
		return config.WebGrant{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.bySite[site]
	return g, ok
}

func (s *grantStore) all() []config.WebGrant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]config.WebGrant, 0, len(s.bySite))
	for _, g := range s.bySite {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site < out[j].Site })
	return out
}

// check refuses a grant naming anything the Host does not offer, or a page tool on a site that does not offer its
// tools, as Start refuses such a configuration. providers answers, for a page origin, whether its site provides.
func (s *grantStore) check(g config.WebGrant, providers providerLookup) error {
	if !config.ValidSite(g.Site) {
		return errors.New("a grant needs one site, like https://example.com")
	}
	if !g.Use && (len(g.Capabilities) > 0 || len(g.Tools) > 0) {
		return errors.New("a site uses nothing until its use switch is on")
	}
	definitions := map[string]capability.Definition{}
	for _, d := range s.registry.List() {
		definitions[d.Name] = d
	}
	for name, target := range g.Capabilities {
		t, err := url.Parse(target)
		if err != nil || t.Host == "" || t.User != nil || t.Scheme != "https" && t.Scheme != "http" {
			return fmt.Errorf("page tool %q needs an absolute HTTP(S) page", name)
		}
		d, ok := definitions[name]
		if !ok || !allowsTarget(d, target) {
			return fmt.Errorf("page tool %q does not work on %s", name, t.Host)
		}
		if !providers.lookup(t.Scheme + "://" + t.Host) {
			return fmt.Errorf("%s does not offer its page tools", t.Host)
		}
	}
	for _, name := range g.Tools {
		if _, ok := s.machine.find(name); !ok {
			return fmt.Errorf("machine tool %q is not offered", name)
		}
	}
	return nil
}

// checkAll checks a whole set of grants against each other.
func (s *grantStore) checkAll(grants map[string]config.WebGrant) error {
	for _, g := range grants {
		if err := s.check(g, providerLookup(grants)); err != nil {
			return err
		}
	}
	return nil
}

func (s *grantStore) checkRemote(g config.RemoteGrant) error {
	if len(g.Tools) == 0 {
		return nil
	}
	pages := map[string]bool{}
	for _, d := range s.registry.List() {
		pages[d.Name] = true
	}
	for _, name := range g.Tools {
		if _, ok := s.machine.find(name); ok {
			continue
		}
		if pages[name] {
			continue
		}
		return fmt.Errorf("remote tool %q is not offered", name)
	}
	return nil
}

// providerLookup answers, for one page origin, whether the site covering it provides its page tools.
type providerLookup map[string]config.WebGrant

func (p providerLookup) lookup(origin string) bool {
	site, err := config.SiteOf(origin)
	return err == nil && p[site].Provide
}

func siteCovers(site, origin string) bool {
	covering, err := config.SiteOf(origin)
	return err == nil && covering == site
}

// set replaces one origin's grant (an empty one removes it) and writes the grants back to the file.
func (s *grantStore) set(g config.WebGrant) error {
	slices.Sort(g.Tools)
	g.Tools = slices.Compact(g.Tools)
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]config.WebGrant{}
	for site, existing := range s.bySite {
		next[site] = existing
	}
	if !g.Use && !g.Provide {
		delete(next, g.Site)
	} else {
		next[g.Site] = g
	}
	// A site that stops providing takes back every lend of its page tools.
	if !g.Provide {
		for site, other := range next {
			kept := map[string]string{}
			for name, target := range other.Capabilities {
				if t, err := url.Parse(target); err != nil || !siteCovers(g.Site, t.Scheme+"://"+t.Host) {
					kept[name] = target
				}
			}
			if len(kept) != len(other.Capabilities) {
				other.Capabilities = kept
				next[site] = other
			}
		}
	}
	if err := s.check(g, providerLookup(next)); err != nil {
		return err
	}
	if err := s.checkAll(next); err != nil {
		return err
	}
	if s.path != "" {
		raw, err := config.Load(s.path)
		if err != nil {
			return err
		}
		raw.Grants.Web = make([]config.WebGrant, 0, len(next))
		for _, kept := range next {
			raw.Grants.Web = append(raw.Grants.Web, kept)
		}
		sort.Slice(raw.Grants.Web, func(i, j int) bool { return raw.Grants.Web[i].Site < raw.Grants.Web[j].Site })
		if _, err := config.Save(s.path, raw); err != nil {
			return fmt.Errorf("save grants: %w", err)
		}
	}
	s.bySite = next
	return nil
}

// setRemote updates one remote wire's grant ("openai" or "http"), persists it,
// and live-updates that wire's MCP tool filter.
func (s *grantStore) setRemote(target string, g config.RemoteGrant) error {
	if target != "openai" && target != "http" {
		return fmt.Errorf("unknown remote grant target %q; choose openai or http", target)
	}
	g.Tools = append([]string(nil), g.Tools...)
	slices.Sort(g.Tools)
	g.Tools = slices.Compact(g.Tools)
	if err := s.checkRemote(g); err != nil {
		return err
	}
	s.mu.Lock()
	next := cloneRemoteGrants(s.remote)
	if target == "openai" {
		next.OpenAI = g
	} else {
		next.HTTP = g
	}
	if s.path != "" {
		raw, err := config.Load(s.path)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		raw.Grants.Remote.OpenAI = next.OpenAI
		raw.Grants.Remote.HTTP = next.HTTP
		if _, err := config.Save(s.path, raw); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("save remote grants: %w", err)
		}
	}
	s.remote = next
	notify := s.onRemoteChange
	s.mu.Unlock()
	if notify != nil {
		notify(target, g)
	}
	s.audit.RecordSystem("system", fmt.Sprintf("Updated %s remote grant (read_only=%t)", target, g.ReadOnly), map[string]any{"target": target, "grant": g})
	return nil
}

// view is what the panel shows: every grant, the catalog grants choose from,
// live remote wires, desktop lock state, and the site of the page it is open on.
func (s *grantStore) view(origin string) map[string]any {
	site, _ := config.SiteOf(origin)
	s.mu.RLock()
	ctrl := s.tunnels
	remote := cloneRemoteGrants(s.remote)
	hasUI := s.uiBootstrap != nil
	s.mu.RUnlock()

	tunnels := []TunnelStatus{}
	attached := 0
	if ctrl != nil {
		tunnels = ctrl.Status()
		attached = ctrl.AttachedClients()
	}
	desktopOwned := false
	if s.controller != nil {
		if st, err := s.controller.Control(context.Background(), computer.ControlOptions{Kind: "status"}); err == nil && st.Status == "owned" {
			desktopOwned = true
		}
	}
	return map[string]any{
		"site":         site,
		"grants":       s.all(),
		"pageTools":    s.registry.List(),
		"machine":      s.machine.list(),
		"proposals":    s.registry.Proposals(),
		"tunnels":      tunnels,
		"remoteGrants": remote,
		"runtime": map[string]any{
			"attached_clients": attached,
			"desktop_owned":    desktopOwned,
			"ui_available":     hasUI,
		},
	}
}

func (s *grantStore) handle(method, origin string, body json.RawMessage) (any, error) {
	switch method {
	case "ui_bootstrap":
		s.mu.RLock()
		fn := s.uiBootstrap
		s.mu.RUnlock()
		if fn == nil {
			return nil, errors.New("local studio UI is unavailable in this mode")
		}
		url, err := fn()
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url}, nil
	case "approve", "reject":
		var name string
		if json.Unmarshal(body, &name) != nil {
			return nil, errors.New("invalid proposal name")
		}
		// Only the owner, through the extension's credential, reaches this; the AI that proposed cannot approve.
		act := s.registry.Approve
		if method == "reject" {
			act = s.registry.Reject
		}
		if err := act(name); err != nil {
			return nil, err
		}
		s.audit.RecordSystem("proposal", fmt.Sprintf("Owner %sd page tool %s", method, name), map[string]any{"name": name, "decision": method})
	case "set":
		var g config.WebGrant
		if err := json.Unmarshal(body, &g); err != nil {
			return nil, errors.New("invalid grant")
		}
		if err := s.set(g); err != nil {
			return nil, err
		}
	case "tunnel_up":
		var req struct {
			Name       string `json:"name"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Name == "" || req.TTLSeconds < 0 {
			return nil, errors.New("invalid tunnel_up request")
		}
		s.mu.RLock()
		ctrl := s.tunnels
		s.mu.RUnlock()
		if ctrl == nil {
			return nil, errors.New("remote tunnel control is unavailable in this mode")
		}
		if _, err := ctrl.Up(req.Name, time.Duration(req.TTLSeconds)*time.Second); err != nil {
			return nil, err
		}
	case "tunnel_down":
		var req struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Name == "" {
			return nil, errors.New("invalid tunnel_down request")
		}
		s.mu.RLock()
		ctrl := s.tunnels
		s.mu.RUnlock()
		if ctrl == nil {
			return nil, errors.New("remote tunnel control is unavailable in this mode")
		}
		if _, err := ctrl.Down(req.Name); err != nil {
			return nil, err
		}
	case "set_remote_grant":
		var req struct {
			Target string             `json:"target"`
			Grant  config.RemoteGrant `json:"grant"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, errors.New("invalid remote grant request")
		}
		if err := s.setRemote(req.Target, req.Grant); err != nil {
			return nil, err
		}
	case "stop_desktop":
		if s.controller != nil {
			s.controller.LocalStop()
			s.audit.RecordSystem("system", "Owner revoked desktop physical control", nil)
		}
	}
	return s.view(origin), nil
}

// machineTools lends built-in tools to websites through an in-process MCP session to the Host's own Server, so a
// web call runs exactly what a local MCP client would, with the same validation and locks.
type machineTools struct {
	session       *mcp.ClientSession
	serverSession *mcp.ServerSession
	tools         []capability.Definition
}

func (m *machineTools) close() {
	if m == nil {
		return
	}
	if m.session != nil {
		_ = m.session.Close()
	}
	if m.serverSession != nil {
		_ = m.serverSession.Close()
	}
}

func connectMachineTools(ctx context.Context, server *mcp.Server) (*machineTools, error) {
	clientSide, serverSide := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "web-grants", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		_ = serverSession.Close()
		return nil, err
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		_ = serverSession.Close()
		return nil, err
	}
	m := &machineTools{session: session, serverSession: serverSession}
	for _, t := range listed.Tools {
		// Namespaced tools are page and app tools, granted by their own rules.
		if strings.Contains(t.Name, ".") {
			continue
		}
		input, _ := json.Marshal(t.InputSchema)
		output := json.RawMessage(`{"type":"object"}`)
		if t.OutputSchema != nil {
			output, _ = json.Marshal(t.OutputSchema)
		}
		a := capability.Annotations{}
		if t.Annotations != nil {
			a.ReadOnlyHint = t.Annotations.ReadOnlyHint
			a.IdempotentHint = t.Annotations.IdempotentHint
			a.DestructiveHint = t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint
			a.OpenWorldHint = t.Annotations.OpenWorldHint != nil && *t.Annotations.OpenWorldHint
		}
		m.tools = append(m.tools, capability.Definition{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: input, OutputSchema: output, Annotations: a})
	}
	return m, nil
}

func (m *machineTools) list() []capability.Definition {
	if m == nil {
		return nil
	}
	return m.tools
}

func (m *machineTools) find(name string) (capability.Definition, bool) {
	for _, t := range m.list() {
		if t.Name == name {
			return t, true
		}
	}
	return capability.Definition{}, false
}

// call runs one machine tool and gives back what a page can read: its structured result, or its text.
func (m *machineTools) call(ctx context.Context, name string, input map[string]any) (any, error) {
	result, err := m.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input})
	if err != nil {
		return nil, err
	}
	var text []string
	images := 0
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			text = append(text, v.Text)
		case *mcp.ImageContent:
			images++
		}
	}
	if result.IsError {
		return nil, errors.New(strings.Join(text, "\n"))
	}
	if result.StructuredContent != nil {
		return result.StructuredContent, nil
	}
	out := map[string]any{"text": strings.Join(text, "\n")}
	if images > 0 {
		out["images"] = images
	}
	return out, nil
}
