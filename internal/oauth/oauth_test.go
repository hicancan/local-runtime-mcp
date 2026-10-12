package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testIssuer   = "https://runtime.example.test"
	testClient   = "gemini-fixture"
	testSecret   = "fixture-client-secret-01234567890123456789"
	testOwner    = "fixture-owner-token-0123456789012345678901"
	testRedirect = "https://client.example.test/callback"
	testVerifier = "fixture-pkce-verifier-0123456789012345678901234567890123456789"
)

type fixture struct {
	t      *testing.T
	server *Server
	now    time.Time
}

func testConfig() Config {
	return Config{Issuer: testIssuer, OwnerToken: testOwner, Clients: []Client{
		{ID: testClient, Secret: testSecret, RedirectURIs: []string{testRedirect}},
		{ID: "second-client", Secret: "second-client-secret-012345678901234567890", RedirectURIs: []string{testRedirect}},
	}}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, server: s, now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) advance(duration time.Duration) {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	f.now = f.now.Add(duration)
}

func (f *fixture) request(method, path string, values url.Values, cookies []*http.Cookie, basic bool) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, testIssuer+path, strings.NewReader(values.Encode()))
	if values != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	if basic {
		r.SetBasicAuth(testClient, testSecret)
	}
	r.Header.Set("Origin", testIssuer)
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}

func authorizeParams() url.Values {
	sum := sha256.Sum256([]byte(testVerifier))
	return url.Values{
		"response_type": {"code"}, "client_id": {testClient}, "redirect_uri": {testRedirect},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"resource": {testIssuer + "/mcp"}, "scope": {"mcp"}, "state": {"fixture opaque state"},
	}
}

var ticketPattern = regexp.MustCompile(`name="ticket" value="([^"]+)"`)

func (f *fixture) begin() (string, []*http.Cookie) {
	f.t.Helper()
	w := f.request(http.MethodGet, "/oauth/authorize?"+authorizeParams().Encode(), nil, nil, false)
	if w.Code != http.StatusOK {
		f.t.Fatalf("authorization page status = %d", w.Code)
	}
	matches := ticketPattern.FindStringSubmatch(w.Body.String())
	if len(matches) != 2 {
		f.t.Fatal("authorization page has no approval ticket")
	}
	if strings.Contains(w.Body.String(), testOwner) || strings.Contains(w.Body.String(), testSecret) {
		f.t.Fatal("authorization page exposed configuration credentials")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Name != "__Host-lrmcp-oauth" || cookies[0].Path != "/" {
		f.t.Fatal("approval cookie attributes are unsafe")
	}
	return matches[1], cookies
}

func (f *fixture) approve(ticket string, cookies []*http.Cookie) string {
	f.t.Helper()
	w := f.request(http.MethodPost, "/oauth/authorize", url.Values{"ticket": {ticket}, "owner_token": {testOwner}, "decision": {"approve"}}, cookies, false)
	if w.Code != http.StatusSeeOther {
		f.t.Fatalf("approval status = %d", w.Code)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || u.Scheme+"://"+u.Host+u.Path != testRedirect || u.Query().Get("state") != "fixture opaque state" || u.Query().Get("code") == "" || u.Query().Get("iss") != testIssuer {
		f.t.Fatal("approval did not return a code to the exact registered callback")
	}
	return u.Query().Get("code")
}

func (f *fixture) code() string {
	f.t.Helper()
	ticket, cookies := f.begin()
	return f.approve(ticket, cookies)
}

func TestConsentPolicyAllowsBrowserCallbackChain(t *testing.T) {
	f := newFixture(t)
	w := f.request(http.MethodGet, "/oauth/authorize?"+authorizeParams().Encode(), nil, nil, false)
	policy := w.Header().Get("Content-Security-Policy")
	if w.Code != http.StatusOK || strings.Contains(policy, "form-action") {
		t.Fatal("consent policy would restrict cross-origin callback redirects")
	}
	for _, directive := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("consent policy omitted %s", directive)
		}
	}
	if !strings.Contains(w.Body.String(), `method="post" action="/oauth/authorize"`) || w.Header().Get("Referrer-Policy") != "same-origin" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("consent form lost its same-origin action or confidentiality headers")
	}
}

func exchangeParams(code string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {testVerifier}, "resource": {testIssuer + "/mcp"},
	}
}

type tokenResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func readTokens(t *testing.T, w *httptest.ResponseRecorder) tokenResult {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("token exchange status = %d", w.Code)
	}
	var tokens tokenResult
	if err := json.Unmarshal(w.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.TokenType != "Bearer" || tokens.ExpiresIn != 3600 || tokens.Scope != "mcp" || tokens.AccessToken == tokens.RefreshToken {
		t.Fatal("token response is incomplete")
	}
	return tokens
}

func (f *fixture) tokens() tokenResult {
	f.t.Helper()
	return readTokens(f.t, f.request(http.MethodPost, "/oauth/token", exchangeParams(f.code()), nil, true))
}

func refreshParams(token string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}, "resource": {testIssuer + "/mcp"}}
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, expected string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != status || body.Error != expected {
		t.Fatalf("expected status %d / %s, got status %d / %s", status, expected, w.Code, body.Error)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("OAuth error lacks confidentiality headers")
	}
}

func TestConfigurationValidation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"remote HTTP issuer", func(c *Config) { c.Issuer = "http://runtime.example.test" }},
		{"issuer userinfo", func(c *Config) { c.Issuer = "https://owner@runtime.example.test" }},
		{"issuer path", func(c *Config) { c.Issuer += "/nested" }},
		{"issuer query", func(c *Config) { c.Issuer += "?token=hidden" }},
		{"short owner key", func(c *Config) { c.OwnerToken = "short" }},
		{"short client secret", func(c *Config) { c.Clients[0].Secret = "short" }},
		{"shared owner key", func(c *Config) { c.Clients[0].Secret = c.OwnerToken }},
		{"no clients", func(c *Config) { c.Clients = nil }},
		{"unsafe ID", func(c *Config) { c.Clients[0].ID = "unsafe <id>" }},
		{"duplicate ID", func(c *Config) { c.Clients[1].ID = c.Clients[0].ID }},
		{"remote HTTP redirect", func(c *Config) { c.Clients[0].RedirectURIs = []string{"http://client.example.test/callback"} }},
		{"redirect fragment", func(c *Config) { c.Clients[0].RedirectURIs = []string{testRedirect + "#fragment"} }},
		{"reserved redirect query", func(c *Config) { c.Clients[0].RedirectURIs = []string{testRedirect + "?code=old"} }},
		{"duplicate redirect", func(c *Config) { c.Clients[0].RedirectURIs = []string{testRedirect, testRedirect} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			test.change(&config)
			if _, err := New(config); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	config := testConfig()
	config.Issuer = "http://127.0.0.1:9316/"
	config.Clients[0].RedirectURIs = []string{"http://localhost:12345/callback"}
	if _, err := New(config); err != nil {
		t.Fatalf("loopback HTTP fixture rejected: %v", err)
	}
}

func TestDiscoveryAndApprovalFlow(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		w := f.request(http.MethodGet, path, nil, nil, false)
		var metadata map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &metadata) != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("discovery failed for %s", path)
		}
		if strings.Contains(path, "protected-resource") && metadata["resource"] != testIssuer+"/mcp" {
			t.Fatal("resource discovery has wrong audience")
		}
		if strings.Contains(path, "authorization-server") {
			if metadata["issuer"] != testIssuer || metadata["registration_endpoint"] != nil || metadata["token_endpoint"] != testIssuer+"/oauth/token" || metadata["authorization_response_iss_parameter_supported"] != true {
				t.Fatal("authorization metadata has inaccurate capabilities")
			}
		}
	}
	if w := f.request(http.MethodGet, "/.well-known/openid-configuration", nil, nil, false); w.Code != http.StatusNotFound {
		t.Fatal("OAuth server pretended to be an OIDC provider")
	}
	if !strings.Contains(f.server.Challenge(), testIssuer+"/.well-known/oauth-protected-resource/mcp") {
		t.Fatal("MCP challenge lacks resource discovery URL")
	}
	tokens := f.tokens()
	if !f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("issued access token rejected")
	}
	for _, nonAccess := range []string{testOwner, testSecret, tokens.RefreshToken, "", "unknown", strings.Repeat("x", 300)} {
		if f.server.ValidateBearer(nonAccess) {
			t.Fatal("non-access credential accepted as bearer")
		}
	}
}

func TestAuthorizationRejectsUnsafeRequests(t *testing.T) {
	tests := []struct{ field, value, error string }{
		{"redirect_uri", "https://attacker.example/callback", "invalid_request"},
		{"redirect_uri", testRedirect + "/", "invalid_request"},
		{"client_id", "unknown", "invalid_request"},
		{"response_type", "token", "invalid_request"},
		{"code_challenge_method", "plain", "invalid_request"},
		{"code_challenge", "short", "invalid_request"},
		{"resource", "https://another.example/mcp", "invalid_target"},
		{"resource", "", "invalid_target"},
		{"scope", "mcp admin", "invalid_scope"},
	}
	for _, test := range tests {
		t.Run(test.field+"="+test.value, func(t *testing.T) {
			f := newFixture(t)
			params := authorizeParams()
			params.Set(test.field, test.value)
			w := f.request(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil, nil, false)
			assertError(t, w, http.StatusBadRequest, test.error)
			if w.Header().Get("Location") != "" {
				t.Fatal("invalid authorization request redirected")
			}
		})
	}
	f := newFixture(t)
	params := authorizeParams()
	params.Add("resource", testIssuer+"/mcp")
	assertError(t, f.request(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil, nil, false), http.StatusBadRequest, "invalid_request")
}

func TestApprovalNeedsOwnerAndBoundCookie(t *testing.T) {
	f := newFixture(t)
	ticket, cookies := f.begin()
	values := url.Values{"ticket": {ticket}, "owner_token": {testOwner}, "decision": {"approve"}}
	assertError(t, f.request(http.MethodPost, "/oauth/authorize", values, nil, false), http.StatusBadRequest, "invalid_request")
	wrongCookie := []*http.Cookie{{Name: cookies[0].Name, Value: strings.Repeat("x", 43)}}
	assertError(t, f.request(http.MethodPost, "/oauth/authorize", values, wrongCookie, false), http.StatusBadRequest, "invalid_request")
	r := httptest.NewRequest(http.MethodPost, testIssuer+"/oauth/authorize", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://attacker.example")
	r.AddCookie(cookies[0])
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	assertError(t, w, http.StatusForbidden, "invalid_request")
	values.Set("owner_token", testSecret)
	w = f.request(http.MethodPost, "/oauth/authorize", values, cookies, false)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), testSecret) || strings.Contains(w.Body.String(), testOwner) {
		t.Fatal("invalid owner key approval did not fail confidentially")
	}
	f.approve(ticket, cookies)
	values.Set("owner_token", testOwner)
	assertError(t, f.request(http.MethodPost, "/oauth/authorize", values, cookies, false), http.StatusBadRequest, "invalid_request")
}

func TestCancelAndApprovalExpiry(t *testing.T) {
	f := newFixture(t)
	ticket, cookies := f.begin()
	w := f.request(http.MethodPost, "/oauth/authorize", url.Values{"ticket": {ticket}, "decision": {"deny"}}, cookies, false)
	u, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusSeeOther || u.Query().Get("error") != "access_denied" || u.Query().Get("code") != "" || u.Query().Get("iss") != testIssuer {
		t.Fatal("cancel did not return access_denied")
	}
	ticket, cookies = f.begin()
	f.advance(ticketLifetime)
	assertError(t, f.request(http.MethodPost, "/oauth/authorize", url.Values{"ticket": {ticket}, "owner_token": {testOwner}, "decision": {"approve"}}, cookies, false), http.StatusBadRequest, "invalid_request")
	if len(f.server.tickets) != 0 {
		t.Fatal("expired approval tickets retained")
	}
}

func TestApprovalRequiresExplicitDecision(t *testing.T) {
	f := newFixture(t)
	ticket, cookies := f.begin()
	for _, decision := range []string{"", "allow", "APPROVE", " approve "} {
		values := url.Values{"ticket": {ticket}, "owner_token": {testOwner}}
		if decision != "" {
			values.Set("decision", decision)
		}
		w := f.request(http.MethodPost, "/oauth/authorize", values, cookies, false)
		assertError(t, w, http.StatusBadRequest, "invalid_request")
		if w.Header().Get("Location") != "" || len(f.server.codes) != 0 {
			t.Fatal("missing or invalid decision issued an authorization code")
		}
	}
	// Invalid decisions must not consume the ticket, so the owner can still
	// make an explicit decision on the original consent page.
	f.approve(ticket, cookies)
}

func TestCodeExchangeValidatesEveryBinding(t *testing.T) {
	f := newFixture(t)
	code := f.code()
	for _, mutation := range []struct{ field, value, expected string }{
		{"code_verifier", strings.Repeat("x", 64), "invalid_grant"},
		{"code_verifier", "short", "invalid_grant"},
		{"redirect_uri", "https://attacker.example/callback", "invalid_grant"},
		{"resource", "https://another.example/mcp", "invalid_target"},
		{"code", "unknown-code", "invalid_grant"},
		{"scope", "admin", "invalid_scope"},
	} {
		params := exchangeParams(code)
		params.Set(mutation.field, mutation.value)
		assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, true), http.StatusBadRequest, mutation.expected)
	}
	params := exchangeParams(code)
	params.Set("client_id", testClient)
	params.Set("client_secret", "wrong-secret")
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, false), http.StatusUnauthorized, "invalid_client")
	params.Set("client_id", "second-client")
	params.Set("client_secret", "second-client-secret-012345678901234567890")
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, false), http.StatusBadRequest, "invalid_grant")
	params.Set("client_id", testClient)
	params.Set("client_secret", testSecret)
	tokens := readTokens(t, f.request(http.MethodPost, "/oauth/token", params, nil, false))
	if !f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("failed binding attempts prevented valid owner-approved exchange")
	}
}

func TestCodeExpiryAndOneTimeUse(t *testing.T) {
	f := newFixture(t)
	code := f.code()
	f.advance(codeLifetime)
	assertError(t, f.request(http.MethodPost, "/oauth/token", exchangeParams(code), nil, true), http.StatusBadRequest, "invalid_grant")
	if len(f.server.codes) != 0 {
		t.Fatal("expired authorization code retained")
	}
	code = f.code()
	tokens := readTokens(t, f.request(http.MethodPost, "/oauth/token", exchangeParams(code), nil, true))
	assertError(t, f.request(http.MethodPost, "/oauth/token", exchangeParams(code), nil, true), http.StatusBadRequest, "invalid_grant")
	if f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("authorization code replay did not revoke the issued grant")
	}
}

func TestRefreshRotationReuseRevokesWholeFamily(t *testing.T) {
	f := newFixture(t)
	first := f.tokens()
	second := readTokens(t, f.request(http.MethodPost, "/oauth/token", refreshParams(first.RefreshToken), nil, true))
	if first.RefreshToken == second.RefreshToken || first.AccessToken == second.AccessToken || !f.server.ValidateBearer(second.AccessToken) {
		t.Fatal("refresh did not issue distinct usable tokens")
	}
	assertError(t, f.request(http.MethodPost, "/oauth/token", refreshParams(first.RefreshToken), nil, true), http.StatusBadRequest, "invalid_grant")
	if f.server.ValidateBearer(first.AccessToken) || f.server.ValidateBearer(second.AccessToken) {
		t.Fatal("refresh reuse did not revoke the whole token family")
	}
	assertError(t, f.request(http.MethodPost, "/oauth/token", refreshParams(second.RefreshToken), nil, true), http.StatusBadRequest, "invalid_grant")
}

func TestRefreshBindingExpiryAndClientAuthentication(t *testing.T) {
	f := newFixture(t)
	tokens := f.tokens()
	params := refreshParams(tokens.RefreshToken)
	params.Set("resource", "https://other.example/mcp")
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, true), http.StatusBadRequest, "invalid_target")
	params.Set("resource", testIssuer+"/mcp")
	params.Set("client_id", "second-client")
	params.Set("client_secret", "second-client-secret-012345678901234567890")
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, false), http.StatusBadRequest, "invalid_grant")
	if !f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("another client was allowed to revoke this token family")
	}
	f.advance(accessLifetime)
	if f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("expired access token remained usable")
	}
	refreshed := readTokens(t, f.request(http.MethodPost, "/oauth/token", refreshParams(tokens.RefreshToken), nil, true))
	if !f.server.ValidateBearer(refreshed.AccessToken) {
		t.Fatal("refresh after access expiry failed")
	}
	f.advance(refreshLifetime)
	assertError(t, f.request(http.MethodPost, "/oauth/token", refreshParams(refreshed.RefreshToken), nil, true), http.StatusBadRequest, "invalid_grant")
	if len(f.server.families) != 0 || len(f.server.refresh) != 0 || len(f.server.access) != 0 {
		t.Fatal("expired grants were not cleaned up")
	}
}

func TestRefreshScopeCannotEscalateOrConsumeGrant(t *testing.T) {
	f := newFixture(t)
	tokens := f.tokens()
	params := refreshParams(tokens.RefreshToken)
	params.Set("scope", "mcp admin")
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, true), http.StatusBadRequest, "invalid_scope")
	if !f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("invalid scope revoked an otherwise valid family")
	}
	params.Set("scope", "mcp")
	refreshed := readTokens(t, f.request(http.MethodPost, "/oauth/token", params, nil, true))
	if !f.server.ValidateBearer(refreshed.AccessToken) || refreshed.Scope != "mcp" {
		t.Fatal("fixed mcp scope was not preserved during refresh")
	}
}

func TestTokenStorageLimitsAndRevocationReclamation(t *testing.T) {
	f := newFixture(t)
	code := f.code()
	for index := range familyLimit {
		f.server.families[digest("capacity-family-"+strconv.Itoa(index))] = grantFamily{clientID: testClient, expires: f.now.Add(refreshLifetime)}
	}
	assertError(t, f.request(http.MethodPost, "/oauth/token", exchangeParams(code), nil, true), http.StatusServiceUnavailable, "temporarily_unavailable")
	if len(f.server.access) != 0 || len(f.server.refresh) != 0 {
		t.Fatal("grant-capacity failure partially issued credentials")
	}
	f.server.mu.Lock()
	f.server.revokeFamily(digest("capacity-family-0"))
	f.server.mu.Unlock()
	tokens := readTokens(t, f.request(http.MethodPost, "/oauth/token", exchangeParams(code), nil, true))
	family := f.server.refresh[digest(tokens.RefreshToken)].family
	for index := range refreshLimit - len(f.server.refresh) {
		f.server.refresh[digest("capacity-refresh-"+strconv.Itoa(index))] = refreshGrant{family: family}
	}
	assertError(t, f.request(http.MethodPost, "/oauth/token", refreshParams(tokens.RefreshToken), nil, true), http.StatusServiceUnavailable, "temporarily_unavailable")
	if len(f.server.refresh) != refreshLimit || f.server.refresh[digest(tokens.RefreshToken)].used || !f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("refresh-capacity failure consumed the grant or exceeded storage bounds")
	}
	w := f.request(http.MethodPost, "/oauth/revoke", url.Values{"token": {tokens.RefreshToken}}, nil, true)
	if w.Code != http.StatusOK || len(f.server.refresh) != 0 || len(f.server.access) != 0 || len(f.server.families) != familyLimit-1 {
		t.Fatal("revocation did not release all storage used by its token family")
	}
	f.advance(refreshLifetime)
	if f.server.ValidateBearer(tokens.AccessToken) || len(f.server.families) != 0 {
		t.Fatal("expired families were not reclaimed")
	}
}

func TestExplicitRevocationDoesNotLeakTokenOwnership(t *testing.T) {
	f := newFixture(t)
	tokens := f.tokens()
	params := url.Values{"token": {tokens.RefreshToken}, "client_id": {"second-client"}, "client_secret": {"second-client-secret-012345678901234567890"}}
	w := f.request(http.MethodPost, "/oauth/revoke", params, nil, false)
	if w.Code != http.StatusOK || !f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("another client revoked an unrelated grant")
	}
	for _, token := range []string{"unknown", tokens.AccessToken} {
		w := f.request(http.MethodPost, "/oauth/revoke", url.Values{"token": {token}}, nil, true)
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Fatal("revocation exposed token existence")
		}
	}
	if f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("revoked grant remained usable")
	}
	assertError(t, f.request(http.MethodPost, "/oauth/token", refreshParams(tokens.RefreshToken), nil, true), http.StatusBadRequest, "invalid_grant")
	if len(f.server.families) != 0 || len(f.server.refresh) != 0 || len(f.server.access) != 0 {
		t.Fatal("revoked token family continued occupying grant storage")
	}
}

func TestEndpointMethodBodyAndAuthenticationConstraints(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/oauth/token", "/oauth/revoke"} {
		assertError(t, f.request(http.MethodGet, path, nil, nil, true), http.StatusMethodNotAllowed, "invalid_request")
	}
	assertError(t, f.request(http.MethodPost, "/.well-known/oauth-authorization-server", nil, nil, false), http.StatusMethodNotAllowed, "invalid_request")
	assertError(t, f.request(http.MethodPost, "/oauth/token", nil, nil, true), http.StatusBadRequest, "invalid_request")
	params := exchangeParams(f.code())
	params.Add("resource", testIssuer+"/mcp")
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, true), http.StatusBadRequest, "invalid_request")
	params = exchangeParams(f.code())
	params.Set("padding", strings.Repeat("x", formLimit))
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, true), http.StatusBadRequest, "invalid_request")
	params = exchangeParams(f.code())
	params.Set("client_secret", testSecret)
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, true), http.StatusUnauthorized, "invalid_client")
	params = url.Values{"grant_type": {"client_credentials"}, "resource": {testIssuer + "/mcp"}}
	assertError(t, f.request(http.MethodPost, "/oauth/token", params, nil, true), http.StatusBadRequest, "unsupported_grant_type")
}

func TestApprovalStorageIsBoundedAndExpires(t *testing.T) {
	f := newFixture(t)
	for range pendingLimit {
		w := f.request(http.MethodGet, "/oauth/authorize?"+authorizeParams().Encode(), nil, nil, false)
		if w.Code != http.StatusOK {
			t.Fatalf("approval before capacity returned %d", w.Code)
		}
	}
	assertError(t, f.request(http.MethodGet, "/oauth/authorize?"+authorizeParams().Encode(), nil, nil, false), http.StatusServiceUnavailable, "temporarily_unavailable")
	f.advance(ticketLifetime)
	if w := f.request(http.MethodGet, "/oauth/authorize?"+authorizeParams().Encode(), nil, nil, false); w.Code != http.StatusOK || len(f.server.tickets) != 1 {
		t.Fatal("expired tickets did not free bounded storage")
	}
}

func TestConcurrentCodeExchangeIsOneTime(t *testing.T) {
	f := newFixture(t)
	code := f.code()
	var group sync.WaitGroup
	statuses := make(chan int, 16)
	for range 16 {
		group.Go(func() {
			w := f.request(http.MethodPost, "/oauth/token", exchangeParams(code), nil, true)
			statuses <- w.Code
		})
	}
	group.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		} else if status != http.StatusBadRequest {
			t.Fatalf("unexpected concurrent exchange status %d", status)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent code exchanges succeeded %d times", successes)
	}
}

func TestConcurrentRefreshAndBearerValidation(t *testing.T) {
	f := newFixture(t)
	tokens := f.tokens()
	var group sync.WaitGroup
	statuses := make(chan int, 16)
	for range 16 {
		group.Go(func() {
			_ = f.server.ValidateBearer(tokens.AccessToken)
			w := f.request(http.MethodPost, "/oauth/token", refreshParams(tokens.RefreshToken), nil, true)
			statuses <- w.Code
		})
	}
	group.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		} else if status != http.StatusBadRequest {
			t.Fatalf("unexpected concurrent refresh status %d", status)
		}
	}
	if successes != 1 || f.server.ValidateBearer(tokens.AccessToken) {
		t.Fatal("refresh race reused a token or kept replayed family valid")
	}
}
