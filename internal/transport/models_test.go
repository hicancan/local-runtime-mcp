package transport

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestModelRoutesRemainSeparateFromMachineAuth(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "private"}[private], func(t *testing.T) {
			called := 0
			model := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called++
				if r.Header.Get("Authorization") != "Bearer model-only-key" {
					w.WriteHeader(401)
					return
				}
				w.WriteHeader(204)
			})
			instance, err := NewHTTP(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil), HTTPConfig{Listen: "127.0.0.1:0", BearerToken: testBearerToken, InternalToken: private, Models: model})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- instance.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			endpoint := "http://" + instance.Address()
			modelResponse := request(t, endpoint+"/v1/models", "model-only-key", "", "")
			modelResponse.Body.Close()
			if private {
				if modelResponse.StatusCode != 404 || called != 0 {
					t.Fatal("private MCP hop exposed model API")
				}
				return
			}
			if modelResponse.StatusCode != 204 {
				t.Fatal("model credential cannot reach model API", modelResponse.StatusCode)
			}
			ownerResponse := request(t, endpoint+"/v1/models", testBearerToken, "", "")
			ownerResponse.Body.Close()
			if ownerResponse.StatusCode != 401 {
				t.Fatal("machine credential accepted for model API")
			}
			mcpResponse := request(t, endpoint+"/mcp", "model-only-key", "", "")
			mcpResponse.Body.Close()
			if mcpResponse.StatusCode != 401 {
				t.Fatal("model credential accepted for machine control")
			}
			hostile := request(t, endpoint+"/v1/models", "model-only-key", "attacker.example", "")
			hostile.Body.Close()
			if hostile.StatusCode != 403 {
				t.Fatal("model route bypassed host validation")
			}
		})
	}
}
