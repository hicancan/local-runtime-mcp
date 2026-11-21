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

	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// grantStore holds the owner's web grants while the Host runs. The configuration file stays their one source: a
// change from the extension's panel takes effect at once and is written back into that file, touching nothing else
// in it (environment overrides and secrets never leave memory).
type grantStore struct {
	mu       sync.RWMutex
	bySite   map[string]config.WebGrant
	path     string
	registry *capability.Registry
	models   []map[string]any
	machine  *machineTools
}

func newGrantStore(grants []config.WebGrant, path string, registry *capability.Registry, models []map[string]any, machine *machineTools) *grantStore {
	store := &grantStore{bySite: map[string]config.WebGrant{}, path: path, registry: registry, models: models, machine: machine}
	for _, g := range grants {
		store.bySite[g.Site] = g
	}
	return store
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
	if !g.Use && (len(g.Models) > 0 || len(g.Capabilities) > 0 || len(g.Tools) > 0) {
		return errors.New("a site uses nothing until its use switch is on")
	}
	for _, id := range g.Models {
		if !slices.ContainsFunc(s.models, func(m map[string]any) bool { return m["id"] == id }) {
			return fmt.Errorf("model %q is not configured", id)
		}
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
	slices.Sort(g.Models)
	g.Models = slices.Compact(g.Models)
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

// view is what the panel shows: every grant, the catalog grants choose from, and the site of the page it is open on.
func (s *grantStore) view(origin string) map[string]any {
	site, _ := config.SiteOf(origin)
	return map[string]any{
		"site":      site,
		"grants":    s.all(),
		"pageTools": s.registry.List(),
		"machine":   s.machine.list(),
		"proposals": s.registry.Proposals(),
		"models":    s.models,
	}
}

func (s *grantStore) handle(method, origin string, body json.RawMessage) (any, error) {
	if method == "approve" || method == "reject" {
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
	}
	if method == "set" {
		var g config.WebGrant
		if err := json.Unmarshal(body, &g); err != nil {
			return nil, errors.New("invalid grant")
		}
		if err := s.set(g); err != nil {
			return nil, err
		}
	}
	return s.view(origin), nil
}

// machineTools lends built-in tools to websites through an in-process MCP session to the Host's own Server, so a
// web call runs exactly what a local MCP client would, with the same validation and locks.
type machineTools struct {
	session *mcp.ClientSession
	tools   []capability.Definition
}

func connectMachineTools(ctx context.Context, server *mcp.Server) (*machineTools, error) {
	clientSide, serverSide := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverSide, nil); err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "web-grants", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		return nil, err
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	m := &machineTools{session: session}
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
