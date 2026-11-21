package runtimehost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/modelapi"
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
	original := "browser:\n  token: \"kept-browser-token-0123456789abcdefghij\"\ngrants:\n  web:\n    - site: https://old.example\n      models: []\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := capability.Load("", &unavailableTabs{})
	if err != nil {
		t.Fatal(err)
	}
	store := newGrantStore(nil, path, registry, nil, fixtureMachine(t))
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
	service, err := modelapi.New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := newGrantStore([]config.WebGrant{{Site: "https://studev.example", Use: true, Tools: []string{"echo_read"}}}, "", registry, nil, fixtureMachine(t))
	handler := webHandler(store, registry, service)
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
	store := newGrantStore(nil, path, registry, nil, fixtureMachine(t))
	// Authorizing with nothing ticked beneath is kept, in the file too.
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
	// Turning providing off takes the lend back; lending again is refused until it is on.
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
