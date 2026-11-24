package transport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/oauth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/html"
)

const (
	oauthFixtureIssuer  = "https://mcp.example.test"
	oauthFixtureClient  = "integration-client"
	oauthFixtureSecret  = "client-secret-0123456789abcdef0123456789abcdef"
	oauthFixtureReturn  = "https://client.example.test/oauth/callback"
	privateFixtureToken = "private-hop-0123456789abcdef0123456789abcdef"
	staticFixtureToken  = "static-public-0123456789abcdef0123456789abcdef"
)

func oauthFixtureConfig() oauth.Config {
	return oauth.Config{
		Issuer: oauthFixtureIssuer, OwnerToken: testBearerToken,
		Clients: []oauth.Client{{ID: oauthFixtureClient, Secret: oauthFixtureSecret, RedirectURIs: []string{oauthFixtureReturn}}},
	}
}

func TestHTTPOAuthApprovalPKCEAndMCP(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "oauth-integration-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "Return pong."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct {
		Value string `json:"value"`
	}, error) {
		return nil, struct {
			Value string `json:"value"`
		}{Value: "pong"}, nil
	})
	auth := oauthFixtureConfig()
	public := startHTTPFixture(t, server, HTTPConfig{
		Listen: "127.0.0.1:0", PublicHost: "mcp.example.test", BearerToken: staticFixtureToken, OAuth: &auth,
	})
	private := startHTTPFixture(t, server, HTTPConfig{
		Listen: "127.0.0.1:0", BearerToken: privateFixtureToken, InternalToken: true,
	})
	endpoint := "http://" + public.Address() + "/mcp"

	for _, credential := range []string{"", testBearerToken, staticFixtureToken, oauthFixtureSecret, privateFixtureToken} {
		response := request(t, endpoint, credential, "mcp.example.test", "")
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), `resource_metadata="`+oauthFixtureIssuer+`/.well-known/oauth-protected-resource`) {
			t.Fatalf("public OAuth listener accepted non-access credential, or omitted discovery: status=%d challenge=%q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
		}
	}
	metadata := oauthFixtureRequest(t, public, http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	var resource struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(metadata.Body).Decode(&resource); err != nil {
		t.Fatal(err)
	}
	metadata.Body.Close()
	if metadata.StatusCode != http.StatusOK || resource.Resource != oauthFixtureIssuer+"/mcp" || len(resource.AuthorizationServers) != 1 || resource.AuthorizationServers[0] != oauthFixtureIssuer {
		t.Fatalf("protected resource metadata = %+v, status=%d", resource, metadata.StatusCode)
	}
	verifier := strings.Repeat("v", 64)
	access := approveOAuthFixture(t, public, verifier)
	client := mcp.NewClient(&mcp.Implementation{Name: "oauth-test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Timeout: 5 * time.Second, Transport: bearerRoundTripper{base: http.DefaultTransport, token: access}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("OAuth MCP initialize: %v", err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "ping" {
		t.Fatalf("OAuth MCP tools/list: %+v %v", listed, err)
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("OAuth MCP tools/call: %+v %v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil || string(encoded) != `{"value":"pong"}` {
		t.Fatalf("OAuth MCP result: %s %v", encoded, err)
	}

	// An OAuth access token is not a credential for the OpenAI provider hop.
	denied := request(t, "http://"+private.Address()+"/mcp", access, "", "")
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("private provider accepted public OAuth access: %d", denied.StatusCode)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+private.Address()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(InternalTokenHeader, access)
	denied, err = (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("private hop accepted OAuth access as internal token: %d", denied.StatusCode)
	}
	req.Header.Set(InternalTokenHeader, privateFixtureToken)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	accepted, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	accepted.Body.Close()
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("private credential did not admit forwarded OAuth metadata: %d", accepted.StatusCode)
	}

	// Static/public origin protection still applies to authenticated MCP calls.
	blocked := request(t, endpoint, access, "mcp.example.test", "https://client.example.test")
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden {
		t.Fatalf("OAuth MCP browser origin status = %d", blocked.StatusCode)
	}
}

func TestHTTPOAuthAllRoutesValidateHost(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "oauth-host-test", Version: "1"}, nil)
	auth := oauthFixtureConfig()
	instance := startHTTPFixture(t, server, HTTPConfig{Listen: "127.0.0.1:0", PublicHost: "mcp.example.test", OAuth: &auth})
	for _, path := range []string{"/mcp", "/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server", "/oauth/authorize", "/oauth/token", "/unrecognized"} {
		req, err := http.NewRequest(http.MethodGet, "http://"+instance.Address()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "untrusted.example.test"
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("host protection for %s: %d", path, response.StatusCode)
		}
	}
}

func TestHTTPOAuthInvalidConfigurationDoesNotBind(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "oauth-config-test", Version: "1"}, nil)
	good := oauthFixtureConfig()
	invalidOwner := good
	invalidOwner.OwnerToken = "short"
	for _, cfg := range []HTTPConfig{
		{PublicHost: "mcp.example.test", InternalToken: true, BearerToken: privateFixtureToken, OAuth: &good},
		{OAuth: &good},
		{PublicHost: "other.example.test", OAuth: &good},
		{PublicHost: "mcp.example.test", OAuth: &invalidOwner},
	} {
		reservation, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Listen = reservation.Addr().String()
		if err := reservation.Close(); err != nil {
			t.Fatal(err)
		}
		instance, err := NewHTTP(server, cfg)
		if err == nil || instance != nil {
			t.Fatal("invalid HTTP OAuth configuration accepted")
		}
		probe, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			t.Fatalf("failed HTTP constructor leaked its listener: %v", err)
		}
		probe.Close()
	}
}

func approveOAuthFixture(t *testing.T, instance *HTTPServer, verifier string) string {
	t.Helper()
	challenge := sha256.Sum256([]byte(verifier))
	authorization := url.Values{
		"response_type": {"code"}, "client_id": {oauthFixtureClient}, "redirect_uri": {oauthFixtureReturn},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
		"resource": {oauthFixtureIssuer + "/mcp"}, "scope": {"mcp"}, "state": {"integration-state"},
	}
	response := oauthFixtureRequest(t, instance, http.MethodGet, "/oauth/authorize?"+authorization.Encode(), nil)
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("OAuth authorization form status = %d", response.StatusCode)
	}
	ticket := hiddenOAuthTicket(t, response.Body)
	response.Body.Close()
	form := url.Values{"ticket": {ticket}, "owner_token": {testBearerToken}, "decision": {"approve"}}
	req, err := http.NewRequest(http.MethodPost, "http://"+instance.Address()+"/oauth/authorize", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "mcp.example.test"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", oauthFixtureIssuer)
	for _, cookie := range response.Cookies() {
		// Cloudflare terminates public TLS; this fixture calls the private HTTP
		// socket directly, while preserving the public Host and browser cookie.
		req.AddCookie(cookie)
	}
	approved, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	approved.Body.Close()
	callback, err := url.Parse(approved.Header.Get("Location"))
	if err != nil || approved.StatusCode != http.StatusSeeOther || callback.Query().Get("state") != "integration-state" || callback.Query().Get("code") == "" {
		t.Fatalf("OAuth owner approval failed: status=%d", approved.StatusCode)
	}
	form = url.Values{
		"grant_type": {"authorization_code"}, "code": {callback.Query().Get("code")}, "redirect_uri": {oauthFixtureReturn},
		"code_verifier": {verifier}, "resource": {oauthFixtureIssuer + "/mcp"},
	}
	req, err = http.NewRequest(http.MethodPost, "http://"+instance.Address()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "mcp.example.test"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(oauthFixtureClient, oauthFixtureSecret)
	token, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer token.Body.Close()
	var payload struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Refresh     string `json:"refresh_token"`
	}
	if err := json.NewDecoder(token.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if token.StatusCode != http.StatusOK || payload.AccessToken == "" || !strings.EqualFold(payload.TokenType, "bearer") || payload.ExpiresIn <= 0 || payload.Refresh == "" {
		t.Fatalf("OAuth code exchange failed: status=%d", token.StatusCode)
	}
	return payload.AccessToken
}

func hiddenOAuthTicket(t *testing.T, reader io.Reader) string {
	t.Helper()
	tokens := html.NewTokenizer(io.LimitReader(reader, 64<<10))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			t.Fatal("OAuth approval form omitted its ticket")
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokens.Token()
			if token.Data != "input" {
				continue
			}
			var name, value string
			for _, attribute := range token.Attr {
				if attribute.Key == "name" {
					name = attribute.Val
				}
				if attribute.Key == "value" {
					value = attribute.Val
				}
			}
			if name == "ticket" && value != "" {
				return value
			}
		}
	}
}

func oauthFixtureRequest(t *testing.T, instance *HTTPServer, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+instance.Address()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "mcp.example.test"
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestHTTPOAuthCoexistsWithNamedRemoteTokensAndHeaders(t *testing.T) {
	var seenMeta audit.CallMeta
	server := mcp.NewServer(&mcp.Implementation{Name: "hybrid-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "Return pong.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct {
		Value string `json:"value"`
	}, error) {
		seenMeta = audit.CallMetaFromContext(ctx)
		return nil, struct {
			Value string `json:"value"`
		}{Value: "pong"}, nil
	})
	auth := oauthFixtureConfig()
	astraToken := "astra-cluster-token-0123456789abcdef0123456789ab"
	public := startHTTPFixture(t, server, HTTPConfig{
		Listen:      "127.0.0.1:0",
		PublicHost:  "mcp.example.test",
		BearerToken: staticFixtureToken,
		OAuth:       &auth,
		Wire:        "cloudflare",
		RemoteTokens: []RemoteTokenAuth{
			{ID: "astra-cluster", Token: astraToken, ReadOnly: true},
		},
	})
	endpoint := "http://" + public.Address() + "/mcp"

	// Legacy static BearerToken is STILL rejected when OAuth is enabled.
	denied := request(t, endpoint, staticFixtureToken, "mcp.example.test", "")
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("legacy bearer_token accepted under OAuth: %d", denied.StatusCode)
	}

	// Explicitly configured named RemoteToken works alongside OAuth and propagates X-LRMCP-Agent + Policy.
	client := mcp.NewClient(&mcp.Implementation{Name: "cloud-runner", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{Timeout: 5 * time.Second, Transport: headerRoundTripper{
			base:  http.DefaultTransport,
			token: astraToken,
			extra: map[string]string{"X-LRMCP-Agent": "astra-3"},
		}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("named remote token connect failed: %v", err)
	}
	defer session.Close()

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("named remote token call failed: %v", err)
	}
	if seenMeta.Wire != "cloudflare" || seenMeta.AgentID != "astra-3" || seenMeta.Policy == nil {
		t.Fatalf("unexpected CallMeta: %+v", seenMeta)
	}
	if !seenMeta.Policy("ping", true, false) || seenMeta.Policy("process_run", false, true) {
		t.Fatal("read-only RemoteToken policy did not enforce read-only boundary")
	}
}

type headerRoundTripper struct {
	base  http.RoundTripper
	token string
	extra map[string]string
}

func (r headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+r.token)
	for k, v := range r.extra {
		clone.Header.Set(k, v)
	}
	return r.base.RoundTrip(clone)
}
