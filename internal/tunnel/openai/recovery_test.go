package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/tunnel-client/testsupport/mocktunnelservice"
)

// Exercise the production provider graph, an actual private MCP HTTP host, and
// the tunnel wire protocol. Faults affect only one request or poll cycle.
func TestOfficialProviderTransientFailureRecovery(t *testing.T) {
	for _, failure := range []string{"poll_503", "forward_http_disconnect", "response_post_503"} {
		t.Run(failure, func(t *testing.T) {
			aStarted, bStarted := make(chan struct{}), make(chan struct{})
			aRelease, bRelease := make(chan struct{}), make(chan struct{})
			faultSeen := make(chan struct{})
			var releaseAOnce, releaseBOnce, faultOnce sync.Once
			releaseA := func() { releaseAOnce.Do(func() { close(aRelease) }) }
			releaseB := func() { releaseBOnce.Do(func() { close(bRelease) }) }
			markFault := func() { faultOnce.Do(func() { close(faultSeen) }) }
			var aToolCalls, bToolCalls, cToolCalls atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "recovery-test", Version: "1"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo after a test-controlled gate."},
				func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
					Value string `json:"value"`
				}) (*mcp.CallToolResult, struct {
					Value string `json:"value"`
				}, error) {
					var gate <-chan struct{}
					switch in.Value {
					case "A":
						aToolCalls.Add(1)
						close(aStarted)
						gate = aRelease
					case "B":
						bToolCalls.Add(1)
						close(bStarted)
						gate = bRelease
					case "C":
						cToolCalls.Add(1)
					}
					if gate != nil {
						select {
						case <-gate:
						case <-ctx.Done():
							return nil, struct {
								Value string `json:"value"`
							}{}, ctx.Err()
						}
					}
					return nil, struct {
						Value string `json:"value"`
					}{Value: in.Value}, nil
				})

			const token = "0123456789abcdef0123456789abcdef"
			host, err := transport.NewHTTP(server, transport.HTTPConfig{
				Listen: "127.0.0.1:0", BearerToken: token, InternalToken: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			hostContext, cancelHost := context.WithCancel(context.Background())
			hostDone := make(chan error, 1)
			go func() { hostDone <- host.Run(hostContext) }()
			t.Cleanup(func() {
				cancelHost()
				select {
				case err := <-hostDone:
					if err != nil {
						t.Errorf("host shutdown: %v", err)
					}
				case <-time.After(8 * time.Second):
					t.Error("host shutdown exceeded bound")
				}
			})
			hostURL, _ := url.Parse("http://" + host.Address())
			proxy := httputil.NewSingleHostReverseProxy(hostURL)
			forwardingEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failure == "forward_http_disconnect" && r.Method == http.MethodPost {
					body, err := io.ReadAll(r.Body)
					_ = r.Body.Close()
					if err != nil {
						t.Error(err)
						http.Error(w, "read failed", http.StatusBadRequest)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var request struct {
						Params struct {
							Arguments struct{ Value string }
						}
					}
					_ = json.Unmarshal(body, &request)
					if request.Params.Arguments.Value == "A" {
						close(aStarted)
						select {
						case <-aRelease:
						case <-r.Context().Done():
							return
						}
						// Close the request's actual TCP connection before returning
						// HTTP headers; the MCP host and other connections stay live.
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						markFault()
						return
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(forwardingEndpoint.Close)

			type response struct {
				RequestID    string          `json:"request_id"`
				JSONResponse json.RawMessage `json:"resp_json"`
				ResponseCode int             `json:"resp_code"`
			}
			commands := make(chan json.RawMessage, 8)
			responses := make(chan response, 8)
			var deliveredCommands, pollFailed atomic.Bool
			var aPostAttempts atomic.Int32
			controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer mock-api-key" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/poll"):
					if failure == "poll_503" && deliveredCommands.Load() && pollFailed.CompareAndSwap(false, true) {
						http.Error(w, "transient poll failure", http.StatusServiceUnavailable)
						markFault()
						return
					}
					batch := []json.RawMessage{}
					select {
					case command := <-commands:
						batch = append(batch, command)
					case <-time.After(20 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					for len(batch) < cap(commands) {
						select {
						case command := <-commands:
							batch = append(batch, command)
						default:
							goto send
						}
					}
				send:
					_ = json.NewEncoder(w).Encode(map[string]any{"commands": batch})
					if len(batch) > 0 {
						deliveredCommands.Store(true)
					}
				case strings.HasSuffix(r.URL.Path, "/response"):
					var result response
					if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
						t.Error(err)
						http.Error(w, "invalid response", http.StatusBadRequest)
						return
					}
					if result.RequestID == "A" && aPostAttempts.Add(1) == 1 && failure == "response_post_503" {
						http.Error(w, "transient response failure", http.StatusServiceUnavailable)
						markFault()
						return
					}
					select {
					case responses <- result:
					case <-r.Context().Done():
						return
					}
					_, _ = w.Write([]byte(`{"status":"ok"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(controlPlane.Close)
			baseURL, _ := url.Parse(controlPlane.URL)
			endpointURL, _ := url.Parse(forwardingEndpoint.URL + "/mcp")
			provider := newProvider(forwardingConfig(Config{TunnelID: testTunnelID, APIKey: "mock-api-key"}, baseURL, endpointURL, token))
			t.Cleanup(func() {
				releaseA()
				releaseB()
				stopContext, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				if err := provider.Stop(stopContext); err != nil {
					t.Errorf("provider shutdown: %v", err)
				}
			})
			startContext, cancelStart := context.WithTimeout(context.Background(), 8*time.Second)
			err = provider.Start(startContext)
			cancelStart()
			if err != nil {
				t.Fatal(err)
			}
			providerDone := provider.Wait()
			add := func(value string) {
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{
					"_meta": map[string]any{mcp.MetaKeyProtocolVersion: "2026-07-28", mcp.MetaKeyClientCapabilities: map[string]any{}},
					"name":  "echo", "arguments": map[string]any{"value": value},
				}})
				if err != nil {
					t.Fatal(err)
				}
				commands <- mocktunnelservice.NewCommand(value, raw, http.Header{
					"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"},
					"Mcp-Protocol-Version": {"2026-07-28"},
				})
			}
			wait := func(signal <-chan struct{}, name string) {
				t.Helper()
				select {
				case <-signal:
				case <-time.After(8 * time.Second):
					t.Fatalf("timed out waiting for %s", name)
				}
			}
			await := func() response {
				t.Helper()
				select {
				case result := <-responses:
					return result
				case <-time.After(8 * time.Second):
					t.Fatal("no terminal response from official dispatcher")
					return response{}
				}
			}
			assertSuccess := func(result response, value string) {
				t.Helper()
				if result.RequestID != value || result.ResponseCode != http.StatusOK || !strings.Contains(string(result.JSONResponse), `"value":"`+value+`"`) {
					t.Fatalf("response = %+v, want successful %s", result, value)
				}
			}

			add("A")
			add("B")
			wait(aStarted, "A to enter forwarding")
			wait(bStarted, "parallel B to enter the host")
			if failure != "poll_503" {
				releaseA()
			}
			wait(faultSeen, "the transient failure")
			releaseB()
			if failure == "poll_503" {
				assertSuccess(await(), "B")
				releaseA()
				assertSuccess(await(), "A")
			} else {
				seen := make(map[string]response)
				for range 2 {
					result := await()
					if _, duplicate := seen[result.RequestID]; duplicate {
						t.Fatalf("duplicate terminal response for %s", result.RequestID)
					}
					seen[result.RequestID] = result
				}
				assertSuccess(seen["B"], "B")
				if failure == "response_post_503" {
					assertSuccess(seen["A"], "A")
					if aPostAttempts.Load() != 2 {
						t.Fatalf("A response POST attempts = %d, want exactly 2", aPostAttempts.Load())
					}
				} else {
					var failed struct {
						ID    int `json:"id"`
						Error struct {
							Code int `json:"code"`
							Data struct {
								TunnelFailure struct {
									Source                   string `json:"source"`
									UpstreamResponseReceived bool   `json:"upstream_response_received"`
								} `json:"tunnel_failure"`
							} `json:"data"`
						} `json:"error"`
					}
					if err := json.Unmarshal(seen["A"].JSONResponse, &failed); err != nil {
						t.Fatal(err)
					}
					if seen["A"].ResponseCode != http.StatusBadGateway || failed.ID != 7 || failed.Error.Code != -32603 || failed.Error.Data.TunnelFailure.Source != "transport_closed" || failed.Error.Data.TunnelFailure.UpstreamResponseReceived {
						t.Fatalf("A disconnect response = %+v: %s", seen["A"], seen["A"].JSONResponse)
					}
				}
			}
			add("C")
			assertSuccess(await(), "C")
			select {
			case signal := <-providerDone:
				t.Fatalf("individual failure stopped provider: %+v", signal)
			default:
			}
			select {
			case err := <-hostDone:
				t.Fatalf("individual failure stopped host: %v", err)
			default:
			}
			wantACalls := int32(1)
			if failure == "forward_http_disconnect" {
				wantACalls = 0
			}
			if aToolCalls.Load() != wantACalls || bToolCalls.Load() != 1 || cToolCalls.Load() != 1 {
				t.Fatalf("tool executions A/B/C = %d/%d/%d, want %d/1/1", aToolCalls.Load(), bToolCalls.Load(), cToolCalls.Load(), wantACalls)
			}
		})
	}
}
