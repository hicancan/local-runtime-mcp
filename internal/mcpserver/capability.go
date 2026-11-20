package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerCapabilityTools lists the catalog's page tools and follows it: when the owner approves a proposal the
// catalog reloads, and tools are added, replaced or removed so connected clients see the new list.
func registerCapabilityTools(server *mcp.Server, registry *capability.Registry) {
	var mu sync.Mutex
	registered := map[string]string{}
	follow := func() {
		mu.Lock()
		defer mu.Unlock()
		current := map[string]bool{}
		for _, definition := range registry.List() {
			current[definition.Name] = true
			fingerprint, _ := json.Marshal(definition)
			if registered[definition.Name] == string(fingerprint) {
				continue
			}
			registered[definition.Name] = string(fingerprint)
			addCapabilityTool(server, registry, definition)
		}
		var gone []string
		for name := range registered {
			if !current[name] {
				gone = append(gone, name)
				delete(registered, name)
			}
		}
		if len(gone) > 0 {
			server.RemoveTools(gone...)
		}
	}
	follow()
	registry.Watch(follow)
}

func addCapabilityTool(server *mcp.Server, registry *capability.Registry, definition capability.Definition) {
	input := capabilityInputSchema(definition.InputSchema)
	annotations := definition.Annotations
	registered := tool(definition.Name, definition.Title, definition.Description,
		annotations.ReadOnlyHint, annotations.DestructiveHint, annotations.IdempotentHint, annotations.OpenWorldHint)
	registered.InputSchema = input
	registered.OutputSchema = definition.OutputSchema
	server.AddTool(registered, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input struct {
			BrowserID string         `json:"browser_id"`
			TabID     string         `json:"tab_id"`
			Arguments map[string]any `json:"arguments"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(request.Params.Arguments)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || input.Arguments == nil {
			return capabilityFailure(errors.New("capability input requires browser_id, tab_id and arguments")), nil
		}
		result, err := registry.Call(ctx, definition.Name, input.BrowserID, input.TabID, input.Arguments)
		if err != nil {
			return capabilityFailure(err), nil
		}
		return capabilityResult(result), nil
	})
}

type proposeInput struct {
	Name       string         `json:"name" jsonschema:"page tool name namespace.action, the namespace naming the site, e.g. chaoxing.course_outline"`
	Definition map[string]any `json:"definition" jsonschema:"the tool definition: title, description, origins (exact https origins), inputSchema, outputSchema and all four annotations"`
	Script     string         `json:"script" jsonschema:"a JavaScript function expression run in the page: function (args) { ... return result }"`
}

// registerProposeTool lets an AI that explored a site write down a page tool for the owner to approve.
func registerProposeTool(server *mcp.Server, registry *capability.Registry) {
	propose := inputTool[proposeInput](tool("capability_propose", "Propose page tool",
		"After exploring a website with browser_snapshot and browser_action, write down how to do one task there as a page tool: a definition (title, description, exact origins, input and output JSON schemas, all four annotations) and a page-side JavaScript function. It is validated and stored as a proposal; nothing runs until the owner approves it in the browser extension's panel for that site. Prefer read-only tools that observe a loaded page; declare readOnlyHint false for anything that changes the site. Proposing the same name again replaces the earlier draft.",
		false, false, true, false), func(*jsonschema.Schema) {})
	mcp.AddTool(server, propose, func(_ context.Context, _ *mcp.CallToolRequest, in proposeInput) (*mcp.CallToolResult, any, error) {
		definition, err := json.Marshal(in.Definition)
		if err != nil {
			return capabilityFailure(errors.New("definition must be a JSON object")), nil, nil
		}
		proposal, err := registry.Propose(in.Name, definition, in.Script)
		if err != nil {
			return capabilityFailure(err), nil, nil
		}
		status := "awaiting the owner's approval in the extension panel for " + strings.Join(proposal.Definition.Origins, ", ")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: proposal.Definition.Name + ": " + status}}}, map[string]any{"name": proposal.Definition.Name, "status": status, "replaces": proposal.Replaces}, nil
	})
}

func registerLocalAppTools(server *mcp.Server, registry *capability.Registry) {
	for _, definition := range registry.AppDefinitions() {
		annotations := definition.Annotations
		registered := tool(definition.Name, definition.Title, definition.Description,
			annotations.ReadOnlyHint, annotations.DestructiveHint, annotations.IdempotentHint, annotations.OpenWorldHint)
		registered.InputSchema, registered.OutputSchema = definition.InputSchema, definition.OutputSchema
		server.AddTool(registered, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			input := map[string]any{}
			if len(request.Params.Arguments) > 0 && json.Unmarshal(request.Params.Arguments, &input) != nil {
				return capabilityFailure(errors.New("local app arguments must be an empty object")), nil
			}
			result, err := registry.CallApp(ctx, definition.Name, input)
			if err != nil {
				return capabilityFailure(err), nil
			}
			return capabilityResult(result), nil
		})
	}
}

func capabilityResult(result map[string]any) *mcp.CallToolResult {
	text, err := json.Marshal(result)
	if err != nil {
		return capabilityFailure(errors.New("capability result is not JSON"))
	}
	return &mcp.CallToolResult{StructuredContent: result, Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}
}

func capabilityFailure(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

func capabilityInputSchema(raw json.RawMessage) map[string]any {
	var arguments map[string]any
	_ = json.Unmarshal(raw, &arguments) // The registry already resolved this schema.
	// Local references refer to the original argument schema root, not the
	// browser routing envelope added for MCP. Keep those references meaningful.
	prefixSchemaRefs(arguments)
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"browser_id": map[string]any{"type": "string", "minLength": 1, "description": "Browser instance ID from browser_status"},
			"tab_id":     map[string]any{"type": "string", "minLength": 1, "description": "Opaque tab ID from browser_tabs or browser_open"},
			"arguments":  arguments,
		},
		"required":             []string{"browser_id", "tab_id", "arguments"},
		"additionalProperties": false,
	}
}

func prefixSchemaRefs(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	if id, _ := schema["$id"].(string); id != "" {
		return // An embedded resource retains its own reference root.
	}
	for _, keyword := range []string{"$ref", "$dynamicRef"} {
		if ref, ok := schema[keyword].(string); ok && (ref == "#" || strings.HasPrefix(ref, "#/")) {
			schema[keyword] = "#/properties/arguments" + strings.TrimPrefix(ref, "#")
		}
	}
	// Traverse schema positions only. A $ref property inside default/example
	// data is literal user data and must not be rewritten.
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if children, ok := schema[keyword].(map[string]any); ok {
			for _, child := range children {
				prefixSchemaRefs(child)
			}
		}
	}
	for _, keyword := range []string{"additionalProperties", "unevaluatedProperties", "propertyNames", "items", "contains", "unevaluatedItems", "not", "if", "then", "else", "contentSchema"} {
		prefixSchemaRefs(schema[keyword])
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if children, ok := schema[keyword].([]any); ok {
			for _, child := range children {
				prefixSchemaRefs(child)
			}
		}
	}
}
