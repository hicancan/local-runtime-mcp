package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerCapabilityTools(server *mcp.Server, registry *capability.Registry) {
	for _, definition := range registry.List() {
		definition := definition
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
			text, err := json.Marshal(result)
			if err != nil {
				return capabilityFailure(errors.New("capability result is not JSON")), nil
			}
			return &mcp.CallToolResult{StructuredContent: result, Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}, nil
		})
	}
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
