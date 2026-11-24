package runtimehost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text"`
}

func fixtureMachine(t *testing.T) *machineTools {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo_read", Description: "Echo text.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "heard " + in.Text}}}, nil, nil
		})
	machine, err := connectMachineTools(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func TestGrantChangesGoBackToTheFileAndNothingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-runtime-mcp.yaml")
	original := "browser:\n  token: \"kept-browser-token-0123456789abcdefghij\"\ngrants:\n  web:\n    - site: https://old.example\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := capability.Load("", &unavailableTabs{})
	if err != nil {
		t.Fatal(err)
	}
	store := newGrantStore(nil, config.RemoteGrants{}, path, registry, fixtureMachine(t), nil, nil)
	if err := store.set(config.WebGrant{Site: "https://studev.example", Use: true, Tools: []string{"echo_read"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.set(config.WebGrant{Site: "https://studev.example", Use: true, Tools: []string{"process_run"}}); err == nil {
		t.Fatal("a tool the Host does not offer was granted")
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Browser.Token != "kept-browser-token-0123456789abcdefghij" {
		t.Fatalf("the rest of the file changed: token %q", saved.Browser.Token)
	}
	if len(saved.Grants.Web) != 1 || saved.Grants.Web[0].Site != "https://studev.example" || saved.Grants.Web[0].Tools[0] != "echo_read" {
		t.Fatalf("grants not written back: %+v", saved.Grants.Web)
	}
	if err := store.set(config.WebGrant{Site: "https://studev.example"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.get("https://studev.example"); ok {
		t.Fatal("an empty grant should remove the origin")
	}
}

func TestLentMachineToolsAreListedWithAnnotationsAndRun(t *testing.T) {
	registry, err := capability.Load("", &unavailableTabs{})
	if err != nil {
		t.Fatal(err)
	}
	store := newGrantStore([]config.WebGrant{{Site: "https://studev.example", Use: true, Tools: []string{"echo_read"}}}, config.RemoteGrants{}, "", registry, fixtureMachine(t), nil, nil)
	handler := webHandler(store, registry)
	source := browser.WebSource{Origin: "https://studev.example"}
	listed, err := handler(context.Background(), source, browser.WebRequest{Method: "capabilities.list", Params: json.RawMessage(`{}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions := listed.([]capability.Definition)
	if len(definitions) != 1 || definitions[0].Name != "echo_read" || !definitions[0].Annotations.ReadOnlyHint {
		t.Fatalf("lent tool not listed with its annotation: %+v", definitions)
	}
	result, err := handler(context.Background(), source, browser.WebRequest{Method: "capabilities.invoke", Params: json.RawMessage(`{"name":"echo_read","input":{"text":"hi"}}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.(map[string]any)["text"].(string), "heard hi") {
		t.Fatalf("tool did not run: %+v", result)
	}
	sub := browser.WebSource{Origin: "https://app.studev.example"}
	if _, err := handler(context.Background(), sub, browser.WebRequest{Method: "capabilities.list", Params: json.RawMessage(`{}`)}, nil); err != nil {
		t.Fatalf("a subdomain of the granted site was refused: %v", err)
	}
	other := browser.WebSource{Origin: "https://other.example"}
	if _, err := handler(context.Background(), other, browser.WebRequest{Method: "capabilities.invoke", Params: json.RawMessage(`{"name":"echo_read","input":{}}`)}, nil); err == nil {
		t.Fatal("a site without the grant ran a machine tool")
	}
}

func TestSwitchesAreKeptWithNothingTickedAndProvidingGuardsPageTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-runtime-mcp.yaml")
	if err := os.WriteFile(path, []byte("grants:\n  web: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "icourse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "icourse", "outline.json"), []byte(`{"description":"Read the outline.","inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"origins":["https://www.icourse163.org"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "icourse", "outline.js"), []byte(`return {};`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := capability.Load(dir, &unavailableTabs{})
	if err != nil {
		t.Fatal(err)
	}
	store := newGrantStore(nil, config.RemoteGrants{}, path, registry, fixtureMachine(t), nil, nil)
	if err := store.set(config.WebGrant{Site: "https://icourse163.org", Provide: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.set(config.WebGrant{Site: "https://hicancan.top", Use: true}); err != nil {
		t.Fatal(err)
	}
	saved, _ := config.Load(path)
	if len(saved.Grants.Web) != 2 {
		t.Fatalf("switches without ticks were not kept: %+v", saved.Grants.Web)
	}
	lend := config.WebGrant{Site: "https://hicancan.top", Use: true, Capabilities: map[string]string{"icourse.outline": testCourse}}
	if err := store.set(lend); err != nil {
		t.Fatal(err)
	}
	if err := store.set(config.WebGrant{Site: "https://icourse163.org"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := store.get("https://studev.hicancan.top"); len(g.Capabilities) != 0 || !g.Use {
		t.Fatalf("the lend outlived its provider: %+v", g)
	}
	if err := store.set(lend); err == nil {
		t.Fatal("a page tool was lent from a site that does not provide")
	}
	if err := store.set(config.WebGrant{Site: "https://other.example", Tools: []string{"echo_read"}}); err == nil {
		t.Fatal("tools were granted with the use switch off")
	}
}

type fakeTunnelCtrl struct {
	active map[string]bool
}

func (f *fakeTunnelCtrl) Status() []TunnelStatus {
	out := []TunnelStatus{}
	for _, n := range []string{"openai", "cloudflare", "http"} {
		state := "stopped"
		if f.active[n] {
			state = "active"
		}
		out = append(out, TunnelStatus{Name: n, Configured: true, State: state})
	}
	return out
}
func (f *fakeTunnelCtrl) Up(name string, _ time.Duration) ([]TunnelStatus, error) {
	f.active[name] = true
	return f.Status(), nil
}
func (f *fakeTunnelCtrl) Down(name string) ([]TunnelStatus, error) {
	delete(f.active, name)
	return f.Status(), nil
}
func (f *fakeTunnelCtrl) AttachedClients() int { return 3 }

func TestRemoteGrantsLiveFilterAndExtensionTunnelControls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "local-runtime-mcp.yaml")
	if err := os.WriteFile(path, []byte("browser:\n  listen: 127.0.0.1:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Resolve(path, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	host, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	ctrl := &fakeTunnelCtrl{active: map[string]bool{}}
	host.SetTunnelController(ctrl)

	// Connect an in-memory client to the OpenAI remote server.
	cSide, sSide := mcp.NewInMemoryTransports()
	if _, err := host.RemoteServer("openai").Connect(ctx, sSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "remote-openai", Version: "1"}, nil).Connect(ctx, cSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	full, err := session.ListTools(ctx, nil)
	if err != nil || len(full.Tools) != 22 {
		t.Fatalf("expected 22 remote tools initially, got %d (%v)", len(full.Tools), err)
	}

	// Switch OpenAI remote wire to read-only via the extension grant handler.
	if _, err := host.grants.handle("set_remote_grant", "https://example.com", json.RawMessage(`{"target":"openai","grant":{"read_only":true}}`)); err != nil {
		t.Fatal(err)
	}
	ro, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ro.Tools) == 0 || len(ro.Tools) >= 22 {
		t.Fatalf("expected read-only subset, got %d tools", len(ro.Tools))
	}
	for _, tool := range ro.Tools {
		if tool.Name == "process_run" || tool.Name == "filesystem_write_text" || tool.Name == "computer_action" || tool.Name == "browser_action" {
			t.Fatalf("mutating tool %q leaked into read-only remote wire", tool.Name)
		}
	}

	// Verify persistence in YAML.
	saved, err := config.Load(path)
	if err != nil || !saved.Grants.Remote.OpenAI.ReadOnly {
		t.Fatalf("remote grant not persisted: %+v (%v)", saved.Grants.Remote, err)
	}

	// Toggle tunnel via extension grant handler.
	viewAny, err := host.grants.handle("tunnel_up", "", json.RawMessage(`{"name":"openai","ttl_seconds":60}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(viewAny)
	if !strings.Contains(string(raw), `"attached_clients":3`) || !ctrl.active["openai"] {
		t.Fatalf("extension tunnel_up did not reflect state: %s", raw)
	}
	if _, err := host.grants.handle("tunnel_down", "", json.RawMessage(`{"name":"openai"}`)); err != nil {
		t.Fatal(err)
	}
	if ctrl.active["openai"] {
		t.Fatal("extension tunnel_down did not detach wire")
	}
}
