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
	original := "browser:\n  token: \"kept-browser-token-0123456789abcdefghij\"\ncapabilities:\n  web:\n    - origin: https://old.example\n      models: []\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := capability.Load("", &unavailableTabs{})
	if err != nil {
		t.Fatal(err)
	}
	store := newGrantStore(nil, path, registry, nil, fixtureMachine(t))
	if err := store.set(config.WebGrant{Origin: "https://studev.example", Tools: []string{"echo_read"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.set(config.WebGrant{Origin: "https://studev.example", Tools: []string{"process_run"}}); err == nil {
		t.Fatal("a tool the Host does not offer was granted")
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Browser.Token != "kept-browser-token-0123456789abcdefghij" {
		t.Fatalf("the rest of the file changed: token %q", saved.Browser.Token)
	}
	if len(saved.Capabilities.Web) != 1 || saved.Capabilities.Web[0].Origin != "https://studev.example" || saved.Capabilities.Web[0].Tools[0] != "echo_read" {
		t.Fatalf("grants not written back: %+v", saved.Capabilities.Web)
	}
	if err := store.set(config.WebGrant{Origin: "https://studev.example"}); err != nil {
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
	store := newGrantStore([]config.WebGrant{{Origin: "https://studev.example", Tools: []string{"echo_read"}}}, "", registry, nil, fixtureMachine(t))
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
	other := browser.WebSource{Origin: "https://other.example"}
	if _, err := handler(context.Background(), other, browser.WebRequest{Method: "capabilities.invoke", Params: json.RawMessage(`{"name":"echo_read","input":{}}`)}, nil); err == nil {
		t.Fatal("a site without the grant ran a machine tool")
	}
}
