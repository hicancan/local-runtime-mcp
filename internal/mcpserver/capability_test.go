package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLocalAppToolHasNoServerRouteWithoutStdioActivation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path, _ := json.Marshal(executable)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	definition := fmt.Sprintf(`{
		"description":"Read a local app",
		"command":{"executable":%s,"args":[]},
		"inputSchema":{"type":"object","additionalProperties":false},
		"outputSchema":{"type":"object","additionalProperties":false},
		"annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}
	}`, path)
	if err := os.WriteFile(filepath.Join(root, "fixture", "read.json"), []byte(definition), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := capability.Load(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		local bool
	}{
		{"http and tunnel", false},
		{"stdio", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			go func() { _ = New(Dependencies{Capabilities: registry, LocalApps: tc.local}).Run(ctx, serverTransport) }()
			client := mcp.NewClient(&mcp.Implementation{Name: "app-catalog-fixture", Version: "1"}, nil)
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			listed, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, tool := range listed.Tools {
				if tool.Name == "fixture.read" {
					found = true
				}
			}
			if found != tc.local {
				t.Fatal("app tool visibility does not match its activation")
			}
			if !tc.local {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fixture.read", Arguments: map[string]any{}})
				if err == nil && !result.IsError {
					t.Fatal("unregistered app tool remained callable")
				}
			}
		})
	}
}

type capabilityBrowser struct{ calls int }

func (*capabilityBrowser) Tabs(context.Context, string) ([]browser.Tab, error) {
	return []browser.Tab{{ID: "tab", URL: "https://example.test/course"}}, nil
}

func (b *capabilityBrowser) Act(context.Context, browser.Action) (browser.ActionResult, error) {
	b.calls++
	return browser.ActionResult{Success: true, Value: map[string]any{"text": "page result"}}, nil
}

func TestFileCapabilityIsDiscoverableAndCallableThroughRealMCP(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "example"), 0o755); err != nil {
		t.Fatal(err)
	}
	definition := `{
  "title":"Read an example page","description":"Read the selected example page",
  "origins":["https://example.test"],
  "inputSchema":{"type":"object","$defs":{"text":{"type":"string"}},"properties":{"text":{"$ref":"#/$defs/text"}},"required":["text"],"additionalProperties":false},
  "outputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false},
  "annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":true}
}`
	if err := os.WriteFile(filepath.Join(root, "example", "read.json"), []byte(definition), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "example", "read.js"), []byte("async function(input) { return {text:input.text}; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := &capabilityBrowser{}
	registry, err := capability.Load(root, executor)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = New(Dependencies{Capabilities: registry}).Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "capability-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *mcp.Tool
	for _, registered := range listed.Tools {
		if registered.Name == "example.read" {
			found = registered
		}
	}
	// The 3 core primitives (run, wait, session), capability_propose, and the 1 file capability = 5 tools.
	if found == nil || len(listed.Tools) != 5 || found.Title != "Read an example page" || !found.Annotations.ReadOnlyHint || !*found.Annotations.OpenWorldHint {
		t.Fatal("file capability was not registered alongside the existing tools")
	}
	// The actual advertised schema, including the routing envelope, must retain
	// the capability's local reference semantics for clients consuming tools/list.
	data, _ := json.Marshal(found.InputSchema)
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal("advertised tool schema has a broken reference:", err)
	}
	arguments := map[string]any{"browser_id": "profile", "tab_id": "tab", "arguments": map[string]any{"text": "question"}}
	if err := resolved.Validate(arguments); err != nil {
		t.Fatal("actual tools/list schema rejects valid routed arguments:", err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "example.read", Arguments: arguments})
	if err != nil || result.IsError || executor.calls != 1 {
		t.Fatal("real MCP invocation failed:", err)
	}
	var output map[string]any
	decodeStructured(t, result, &output)
	if output["text"] != "page result" {
		t.Fatal(output)
	}
	if len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != `{"text":"page result"}` {
		t.Fatal("MCP structured output lost its text representation")
	}
	// An AI proposes a tool over MCP; it is not listed until the owner approves it, then connected clients see it.
	proposed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "capability_propose", Arguments: map[string]any{
		"name": "example.again", "script": "async function(input) { return {text:input.text}; }",
		"definition": json.RawMessage(definition),
	}})
	if err != nil || proposed.IsError {
		t.Fatalf("proposal failed: %v %+v", err, proposed)
	}
	listedNames := func() map[string]bool {
		names := map[string]bool{}
		again, _ := session.ListTools(ctx, nil)
		for _, registered := range again.Tools {
			names[registered.Name] = true
		}
		return names
	}
	if listedNames()["example.again"] {
		t.Fatal("a proposal was listed before approval")
	}
	if err := registry.Approve("example.again"); err != nil {
		t.Fatal(err)
	}
	if !listedNames()["example.again"] {
		t.Fatal("an approved tool did not reach the connected client")
	}
	for _, invalid := range []map[string]any{
		{"browser_id": "profile", "tab_id": "tab", "arguments": map[string]any{"text": 7}},
		{"browser_id": "profile", "tab_id": "tab"},
		{"browser_id": "profile", "tab_id": "tab", "arguments": map[string]any{"text": "q"}, "script": "arbitrary"},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "example.read", Arguments: invalid})
		if err != nil || !result.IsError || executor.calls != 1 {
			t.Fatal("invalid tool input executed or failed outside the tool result:", err)
		}
	}
}

func TestRoutedCapabilitySchemaDoesNotRewriteLiteralExamplesOrEmbeddedResources(t *testing.T) {
	for _, source := range []string{
		`{"type":"object","properties":{"data":{"type":"object","default":{"$ref":"#/literal"}}}}`,
		`{"$id":"https://schemas.example.test/arguments","type":"object","$defs":{"text":{"type":"string"}},"properties":{"text":{"$ref":"#/$defs/text"}}}`,
	} {
		wrapped := capabilityInputSchema(json.RawMessage(source))
		data, _ := json.Marshal(wrapped)
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		if _, err := schema.Resolve(nil); err != nil {
			t.Fatal("nested argument schema no longer resolves:", err)
		}
		var original map[string]any
		_ = json.Unmarshal([]byte(source), &original)
		if original["$id"] != nil {
			value := wrapped["properties"].(map[string]any)["arguments"].(map[string]any)
			text := value["properties"].(map[string]any)["text"].(map[string]any)
			if text["$ref"] != "#/$defs/text" {
				t.Fatal("embedded resource references were rebased outside their resource")
			}
		} else {
			value := wrapped["properties"].(map[string]any)["arguments"].(map[string]any)
			literal := value["properties"].(map[string]any)["data"].(map[string]any)["default"].(map[string]any)
			if literal["$ref"] != "#/literal" {
				t.Fatal("literal data was mistaken for a schema reference")
			}
		}
	}
}
