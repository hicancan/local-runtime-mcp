package transport

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testBearerToken = "0123456789abcdef0123456789abcdef"

func TestHTTPTransportAuthenticationHostAndMCP(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "transport-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "Return pong."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct {
		Value string `json:"value"`
	}, error) {
		return nil, struct {
			Value string `json:"value"`
		}{Value: "pong"}, nil
	})
	httpServer, err := NewHTTP(server, HTTPConfig{
		Listen: "127.0.0.1:0", BearerToken: testBearerToken, PublicHost: "mcp.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- httpServer.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("HTTP server shutdown: %v", err)
		}
	})
	endpoint := "http://" + httpServer.Address() + "/mcp"

	response := request(t, endpoint, "", "", "")
	if response.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(response.Header.Get("WWW-Authenticate"), "Bearer ") {
		t.Fatalf("unauthorized response = %d %q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}
	response.Body.Close()

	response = request(t, endpoint, testBearerToken, "wrong.example.com", "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong-host status = %d", response.StatusCode)
	}
	response.Body.Close()

	response = request(t, endpoint, testBearerToken, "mcp.example.com", "https://attacker.example")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("browser-origin status = %d", response.StatusCode)
	}
	response.Body.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "transport-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{
			base: http.DefaultTransport, token: testBearerToken,
		}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", tools.Tools)
	}
}

func TestHTTPTransportRejectsUnsafeConfiguration(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "transport-test", Version: "1"}, nil)
	for _, test := range []HTTPConfig{
		{Listen: "0.0.0.0:9316", BearerToken: testBearerToken},
		{Listen: "127.0.0.1:0", BearerToken: "short"},
		{Listen: "127.0.0.1:0", BearerToken: testBearerToken, PublicHost: "https://mcp.example.com"},
	} {
		if instance, err := NewHTTP(server, test); err == nil {
			t.Fatalf("unsafe configuration was accepted: %+v", test)
		} else if instance != nil {
			t.Fatal("failed constructor returned an instance")
		}
	}
}

func request(t *testing.T, endpoint, token, host, origin string) *http.Response {
	t.Helper()
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	req, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (r bearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+r.token)
	return r.base.RoundTrip(clone)
}
