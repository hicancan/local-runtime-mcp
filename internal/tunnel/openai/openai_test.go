package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/tunnel-client/pkg/runtimeconfig"
	"github.com/openai/tunnel-client/testsupport/mocktunnelservice"
)

const testTunnelID = "tunnel_11111111111111111111111111111111"

func TestForwardingConfiguration(t *testing.T) {
	base, _ := url.Parse("https://example.invalid")
	endpoint, _ := url.Parse("http://127.0.0.1:12345/mcp")
	cfg := forwardingConfig(Config{TunnelID: testTunnelID, APIKey: "mock-api-key"}, base, endpoint, "private")
	if cfg.MCP.TransportKind != runtimeconfig.MCPTransportHTTPStreamable || len(cfg.MCP.ChannelBindings) != 1 ||
		cfg.MCP.ChannelBindings[0].TransportKind != runtimeconfig.MCPTransportHTTPStreamable {
		t.Fatal("OpenAI provider must use the native HTTP forwarding transport")
	}
	if len(cfg.Harpoon.Targets) != 0 || len(cfg.Harpoon.AdditionalTransports) != 0 {
		t.Fatal("unrelated provider surfaces enabled")
	}
}

func TestOfficialDispatcherConcurrentIDsAndDeadlineRecovery(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "dispatcher-test", Version: "1"}, nil)
	cancelled := make(chan struct{}, 1)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo after a bounded delay."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Value string `json:"value"`
			Delay int    `json:"delay"`
		}) (*mcp.CallToolResult, struct {
			Value string `json:"value"`
		}, error) {
			timer := time.NewTimer(time.Duration(in.Delay) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				if in.Value == "deadline" {
					select {
					case cancelled <- struct{}{}:
					default:
					}
				}
				return nil, struct {
					Value string `json:"value"`
				}{}, ctx.Err()
			case <-timer.C:
				return nil, struct {
					Value string `json:"value"`
				}{Value: in.Value}, nil
			}
		})
	// This fixture delivers requests concurrently. The upstream SDK's scripted
	// mock intentionally waits for each response, so it cannot test this invariant.
	commands := make(chan json.RawMessage, 8)
	type response struct {
		RequestID    string          `json:"request_id"`
		JSONResponse json.RawMessage `json:"resp_json"`
		ResponseCode int             `json:"resp_code"`
	}
	responses := make(chan response, 8)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-api-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/poll"):
			var batch []json.RawMessage
			select {
			case command := <-commands:
				batch = append(batch, command)
			case <-time.After(20 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			for len(batch) < 8 {
				select {
				case command := <-commands:
					batch = append(batch, command)
				default:
					goto send
				}
			}
		send:
			_ = json.NewEncoder(w).Encode(map[string]any{"commands": batch})
		case strings.HasSuffix(r.URL.Path, "/response"):
			var result response
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Error(err)
				http.Error(w, "invalid", 400)
				return
			}
			select {
			case responses <- result:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			// No OAuth discovery or Harpoon is part of this provider graph.
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, server, Config{TunnelID: testTunnelID, APIKey: "mock-api-key"}, fixture.URL) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("provider shutdown: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("provider shutdown exceeded bound")
		}
	})
	add := func(id, value string, delay int, deadline, protocol string) {
		meta := map[string]any{}
		if protocol == "2026-07-28" {
			meta[mcp.MetaKeyProtocolVersion] = protocol
			meta[mcp.MetaKeyClientCapabilities] = map[string]any{}
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{
			"_meta": meta,
			"name":  "echo", "arguments": map[string]any{"value": value, "delay": delay},
		}})
		command := mocktunnelservice.NewCommand(id, raw, http.Header{
			"Authorization": {"Bearer caller-auth"},
			"Origin":        {"https://chatgpt.com"},
			"Content-Type":  {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Mcp-Protocol-Version": {protocol},
		})
		if deadline != "" {
			var envelope map[string]any
			_ = json.Unmarshal(command, &envelope)
			envelope["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			envelope["response_timeout"] = deadline
			command, _ = json.Marshal(envelope)
		}
		commands <- command
	}
	add("slow-A", "A", 500, "", "2026-07-28")
	add("fast-B", "B", 10, "", "2026-07-28")
	add("deadline", "deadline", 3000, "300ms", "2026-07-28")
	await := func(id, value string) {
		t.Helper()
		select {
		case result := <-responses:
			if result.RequestID != id || result.ResponseCode != http.StatusOK || !strings.Contains(string(result.JSONResponse), "\"value\":\""+value+"\"") {
				t.Fatalf("response routing/order: got %+v, want %s=%s", result, id, value)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("no response from official dispatcher")
		}
	}
	await("fast-B", "B")
	await("slow-A", "A")
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("request deadline did not cancel only its HTTP call")
	}
	add("after-deadline", "recovered", 1, "", "2026-07-28")
	await("after-deadline", "recovered")
	add("legacy-slow", "old-A", 200, "", "2025-11-25")
	add("legacy-fast", "old-B", 1, "", "2025-11-25")
	await("legacy-fast", "old-B")
	await("legacy-slow", "old-A")
	// The provider deliberately drops expired command responses; no late result
	// should be emitted or consumed by a subsequent request with the same RPC ID.
	select {
	case result := <-responses:
		t.Fatalf("late deadline response: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
}
