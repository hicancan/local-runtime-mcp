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
	byOrigin map[string]config.WebGrant
	path     string
	registry *capability.Registry
	models   []map[string]any
	machine  *machineTools
}

func newGrantStore(grants []config.WebGrant, path string, registry *capability.Registry, models []map[string]any, machine *machineTools) *grantStore {
	store := &grantStore{byOrigin: map[string]config.WebGrant{}, path: path, registry: registry, models: models, machine: machine}
	for _, g := range grants {
		store.byOrigin[g.Origin] = g
	}
	return store
}

func (s *grantStore) get(origin string) (config.WebGrant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.byOrigin[origin]
	return g, ok
}

func (s *grantStore) all() []config.WebGrant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]config.WebGrant, 0, len(s.byOrigin))
	for _, g := range s.byOrigin {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Origin < out[j].Origin })
	return out
}

// check refuses a grant naming anything the Host does not offer, as Start refuses such a configuration.
func (s *grantStore) check(g config.WebGrant) error {
	u, err := url.Parse(g.Origin)
	if err != nil || u.Host == "" || strings.ContainsAny(u.Host, "*\\") || u.User != nil || u.Scheme != "https" && u.Scheme != "http" || g.Origin != u.Scheme+"://"+u.Host {
		return errors.New("a grant needs one exact HTTP(S) origin")
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
	}
	for _, name := range g.Tools {
		if _, ok := s.machine.find(name); !ok {
			return fmt.Errorf("machine tool %q is not offered", name)
		}
	}
	return nil
}

// set replaces one origin's grant (an empty one removes it) and writes the grants back to the file.
func (s *grantStore) set(g config.WebGrant) error {
	slices.Sort(g.Models)
	g.Models = slices.Compact(g.Models)
	slices.Sort(g.Tools)
	g.Tools = slices.Compact(g.Tools)
	if err := s.check(g); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]config.WebGrant{}
	for origin, existing := range s.byOrigin {
		next[origin] = existing
	}
	if len(g.Models) == 0 && len(g.Capabilities) == 0 && len(g.Tools) == 0 {
		delete(next, g.Origin)
	} else {
		next[g.Origin] = g
	}
	if s.path != "" {
		raw, err := config.Load(s.path)
		if err != nil {
			return err
		}
		raw.Capabilities.Web = make([]config.WebGrant, 0, len(next))
		for _, kept := range next {
			raw.Capabilities.Web = append(raw.Capabilities.Web, kept)
		}
		sort.Slice(raw.Capabilities.Web, func(i, j int) bool { return raw.Capabilities.Web[i].Origin < raw.Capabilities.Web[j].Origin })
		if _, err := config.Save(s.path, raw); err != nil {
			return fmt.Errorf("save grants: %w", err)
		}
	}
	s.byOrigin = next
	return nil
}

// view is what the panel shows: every grant, and the catalog grants choose from.
func (s *grantStore) view() map[string]any {
	return map[string]any{
		"grants":    s.all(),
		"pageTools": s.registry.List(),
		"machine":   s.machine.list(),
		"models":    s.models,
	}
}

func (s *grantStore) handle(method string, body json.RawMessage) (any, error) {
	if method == "set" {
		var g config.WebGrant
		if err := json.Unmarshal(body, &g); err != nil {
			return nil, errors.New("invalid grant")
		}
		if err := s.set(g); err != nil {
			return nil, err
		}
	}
	return s.view(), nil
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
