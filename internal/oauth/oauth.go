// Package oauth implements owner-approved OAuth grants for the public MCP
// endpoint. Clients are registered in the runtime configuration; all grants
// belong to this running instance and are invalidated when it exits.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	formLimit       = 16 << 10
	pendingLimit    = 2048
	familyLimit     = 4096
	refreshLimit    = 16384
	ticketLifetime  = 10 * time.Minute
	codeLifetime    = 5 * time.Minute
	accessLifetime  = time.Hour
	refreshLifetime = 30 * 24 * time.Hour
	scope           = "mcp"
)

var (
	clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
)

// Client is a preregistered confidential OAuth client. Redirect URIs are exact
// matches, not patterns; each separate client should have its own secret.
type Client struct {
	ID           string   `yaml:"client_id"`
	Secret       string   `yaml:"client_secret"`
	RedirectURIs []string `yaml:"redirect_uris"`
}

// Config describes the public issuer and the key a machine owner must enter on
// the consent page. OwnerToken is not an MCP bearer token or a client secret.
type Config struct {
	Issuer     string   `yaml:"-"`
	OwnerToken string   `yaml:"owner_token,omitempty"`
	Clients    []Client `yaml:"clients,omitempty"`
}

type registeredClient struct {
	secret    [32]byte
	redirects map[string]bool
}

type authorization struct {
	clientID, redirectURI, challenge, resource, state string
}

type approvalTicket struct {
	authorization
	cookie  [32]byte
	expires time.Time
}

type authorizationCode struct {
	authorization
	expires  time.Time
	consumed bool
	family   [32]byte
}

type grantFamily struct {
	clientID string
	expires  time.Time
}

type accessGrant struct {
	family  [32]byte
	expires time.Time
}

type refreshGrant struct {
	family [32]byte
	used   bool
}

// Server serves OAuth discovery, explicit owner approval, and token endpoints.
// It is safe to share one Server between concurrent HTTP connections.
type Server struct {
	issuer, resource, origin, cookieName string
	secureCookie                         bool
	owner                                [32]byte
	clients                              map[string]registeredClient
	mu                                   sync.Mutex
	now                                  func() time.Time
	tickets                              map[[32]byte]approvalTicket
	codes                                map[[32]byte]authorizationCode
	families                             map[[32]byte]grantFamily
	access                               map[[32]byte]accessGrant
	refresh                              map[[32]byte]refreshGrant
}

// New validates all configuration before creating an authorization server.
func New(config Config) (*Server, error) {
	issuer := strings.TrimSuffix(config.Issuer, "/")
	parsed, err := url.Parse(issuer)
	if err != nil || !validWebURL(parsed) || parsed.Path != "" || parsed.RawQuery != "" {
		return nil, errors.New("OAuth issuer must be a root HTTPS URL (HTTP loopback is supported for local use)")
	}
	if !validSecret(config.OwnerToken) {
		return nil, errors.New("OAuth owner_token must contain at least 32 characters without surrounding whitespace")
	}
	if len(config.Clients) == 0 || len(config.Clients) > 64 {
		return nil, errors.New("OAuth requires between 1 and 64 preregistered clients")
	}
	s := &Server{
		issuer: issuer, resource: issuer + "/mcp", origin: parsed.Scheme + "://" + parsed.Host,
		secureCookie: parsed.Scheme == "https", cookieName: "lrmcp-oauth",
		owner: sha256.Sum256([]byte(config.OwnerToken)), clients: make(map[string]registeredClient),
		now: time.Now, tickets: make(map[[32]byte]approvalTicket), codes: make(map[[32]byte]authorizationCode),
		families: make(map[[32]byte]grantFamily), access: make(map[[32]byte]accessGrant), refresh: make(map[[32]byte]refreshGrant),
	}
	if s.secureCookie {
		s.cookieName = "__Host-lrmcp-oauth"
	}
	for _, client := range config.Clients {
		if !clientIDPattern.MatchString(client.ID) || !validSecret(client.Secret) {
			return nil, errors.New("OAuth client requires a safe client_id and a client_secret of at least 32 characters")
		}
		if subtle.ConstantTimeCompare([]byte(client.Secret), []byte(config.OwnerToken)) == 1 {
			return nil, errors.New("OAuth owner_token and client_secret must be different")
		}
		if _, exists := s.clients[client.ID]; exists {
			return nil, errors.New("OAuth client_id must be unique")
		}
		if len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 16 {
			return nil, errors.New("OAuth client requires between 1 and 16 registered redirect URIs")
		}
		registered := registeredClient{secret: sha256.Sum256([]byte(client.Secret)), redirects: make(map[string]bool)}
		for _, redirect := range client.RedirectURIs {
			u, parseErr := url.Parse(redirect)
			if parseErr != nil || !validWebURL(u) || len(redirect) > 2048 {
				return nil, errors.New("OAuth redirect URI must use HTTPS (HTTP loopback is supported for local use)")
			}
			q, queryErr := url.ParseQuery(u.RawQuery)
			if queryErr != nil || q.Has("code") || q.Has("error") || q.Has("state") || registered.redirects[redirect] {
				return nil, errors.New("OAuth redirect URI contains reserved response parameters or is duplicated")
			}
			registered.redirects[redirect] = true
		}
		s.clients[client.ID] = registered
	}
	return s, nil
}

func validSecret(value string) bool {
	return len(value) >= 32 && len(value) <= 4096 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

func validWebURL(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// Challenge supplies the RFC 9728 protected-resource metadata URL to MCP clients.
func (s *Server) Challenge() string {
	return `Bearer resource_metadata="` + s.issuer + `/.well-known/oauth-protected-resource/mcp", scope="mcp"`
}

// ValidateBearer accepts only live access tokens issued by this server, never
// owner approval keys, client secrets, authorization codes, or refresh tokens.
func (s *Server) ValidateBearer(token string) bool {
	if len(token) == 0 || len(token) > 256 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.cleanup(now)
	grant, ok := s.access[digest(token)]
	if !ok || !now.Before(grant.expires) {
		return false
	}
	family, ok := s.families[grant.family]
	return ok && now.Before(family.expires)
}

// ServeHTTP implements only OAuth 2.0 endpoints, not OpenID Connect discovery.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// Chromium applies form-action to the entire POST redirect chain, including
	// a registered callback's subsequent redirect to its client application.
	// The form has a fixed same-origin action; approval still requires its bound
	// cookie, ticket, Origin and owner key, with an exact registered callback.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		if !method(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"resource": s.resource, "authorization_servers": []string{s.issuer},
			"scopes_supported": []string{scope}, "bearer_methods_supported": []string{"header"},
		})
	case "/.well-known/oauth-authorization-server":
		if !method(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer": s.issuer, "authorization_endpoint": s.issuer + "/oauth/authorize",
			"token_endpoint": s.issuer + "/oauth/token", "revocation_endpoint": s.issuer + "/oauth/revoke",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported":      []string{"client_secret_basic", "client_secret_post"},
			"revocation_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
			"code_challenge_methods_supported":           []string{"S256"}, "scopes_supported": []string{scope},
			"authorization_response_iss_parameter_supported": true,
		})
	case "/oauth/authorize":
		if r.Method == http.MethodGet {
			s.beginApproval(w, r)
		} else if r.Method == http.MethodPost {
			s.approve(w, r)
		} else {
			w.Header().Set("Allow", "GET, POST")
			oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "use GET or POST")
		}
	case "/oauth/token":
		if method(w, r, http.MethodPost) {
			s.token(w, r)
		}
	case "/oauth/revoke":
		if method(w, r, http.MethodPost) {
			s.revoke(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) beginApproval(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > formLimit || !singleValues(q) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid authorization parameters")
		return
	}
	request := authorization{
		clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		challenge: q.Get("code_challenge"), resource: q.Get("resource"), state: q.Get("state"),
	}
	client, ok := s.clients[request.clientID]
	// Unregistered redirect URIs never receive an error redirect.
	if !ok || !client.redirects[request.redirectURI] {
		oauthError(w, http.StatusBadRequest, "invalid_request", "unknown client or redirect URI")
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !validChallenge(request.challenge) || len(request.state) > 2048 {
		oauthError(w, http.StatusBadRequest, "invalid_request", "authorization code with PKCE S256 is required")
		return
	}
	if request.resource != s.resource {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource must identify this MCP endpoint")
		return
	}
	if requestedScope := q.Get("scope"); requestedScope != "" && requestedScope != scope {
		oauthError(w, http.StatusBadRequest, "invalid_scope", "only the mcp scope is supported")
		return
	}
	ticket, err := randomToken("lrmcp_req_")
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not create approval request")
		return
	}
	cookieValue := ""
	if existing, cookieErr := r.Cookie(s.cookieName); cookieErr == nil && validChallenge(existing.Value) {
		cookieValue = existing.Value
	} else if cookieValue, err = randomToken(""); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not create approval session")
		return
	}
	s.mu.Lock()
	now := s.now()
	s.cleanup(now)
	if len(s.tickets) >= pendingLimit {
		s.mu.Unlock()
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many pending approval requests")
		return
	}
	s.tickets[digest(ticket)] = approvalTicket{authorization: request, cookie: digest(cookieValue), expires: now.Add(ticketLifetime)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName, Value: cookieValue, Path: "/", HttpOnly: true,
		Secure: s.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(ticketLifetime.Seconds()),
	})
	s.renderApproval(w, http.StatusOK, ticket, request, "")
}

var approvalPage = template.Must(template.New("approval").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Authorize Local Runtime MCP</title></head>
<body><main><h1>Authorize Local Runtime MCP</h1>
<p>Client <strong>{{.ClientID}}</strong> is requesting access to <strong>{{.Resource}}</strong>.</p>
<p>Approving allows this client to run programs, read and change files, and observe or control this machine's desktop and connected browsers.</p>
<p>Enter the owner approval key from your local runtime configuration only if you initiated this connection and trust the client.</p>
{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}
<form method="post" action="/oauth/authorize" autocomplete="off">
<input type="hidden" name="ticket" value="{{.Ticket}}">
<label>Owner approval key <input type="password" name="owner_token" required autocomplete="off"></label>
<button type="submit" name="decision" value="approve">Approve access</button>
<button type="submit" name="decision" value="deny" formnovalidate>Cancel</button>
</form></main></body></html>`))

func (s *Server) renderApproval(w http.ResponseWriter, status int, ticket string, request authorization, message string) {
	// no-referrer makes Chromium send Origin: null for a non-CORS form POST.
	// same-origin preserves the consent form's Origin while suppressing Referer
	// on the cross-origin callback; protocol and error responses stay no-referrer.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = approvalPage.Execute(w, struct{ Ticket, ClientID, Resource, Error string }{ticket, request.clientID, request.resource, message})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin {
		oauthError(w, http.StatusForbidden, "invalid_request", "approval origin does not match this server")
		return
	}
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	if decision := form.Get("decision"); decision != "approve" && decision != "deny" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "an explicit approval decision is required")
		return
	}
	cookie, err := r.Cookie(s.cookieName)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "approval session is missing")
		return
	}
	ticket := form.Get("ticket")
	s.mu.Lock()
	now := s.now()
	s.cleanup(now)
	request, exists := s.tickets[digest(ticket)]
	cookieHash := digest(cookie.Value)
	if !exists || !now.Before(request.expires) || subtle.ConstantTimeCompare(request.cookie[:], cookieHash[:]) != 1 {
		s.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "invalid_request", "approval request is invalid or expired")
		return
	}
	if form.Get("decision") == "deny" {
		delete(s.tickets, digest(ticket))
		s.mu.Unlock()
		s.authorizationRedirect(w, r, request.authorization, "error", "access_denied")
		return
	}
	ownerHash := digest(form.Get("owner_token"))
	if subtle.ConstantTimeCompare(s.owner[:], ownerHash[:]) != 1 {
		s.mu.Unlock()
		s.renderApproval(w, http.StatusForbidden, ticket, request.authorization, "The owner approval key is incorrect.")
		return
	}
	if len(s.codes) >= pendingLimit {
		s.mu.Unlock()
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many pending authorization codes")
		return
	}
	code, err := randomToken("lrmcp_code_")
	if err != nil {
		s.mu.Unlock()
		oauthError(w, http.StatusInternalServerError, "server_error", "could not create authorization code")
		return
	}
	delete(s.tickets, digest(ticket))
	s.codes[digest(code)] = authorizationCode{authorization: request.authorization, expires: now.Add(codeLifetime)}
	s.mu.Unlock()
	s.authorizationRedirect(w, r, request.authorization, "code", code)
}

func (s *Server) authorizationRedirect(w http.ResponseWriter, r *http.Request, request authorization, key, value string) {
	u, _ := url.Parse(request.redirectURI) // Already validated against immutable registration.
	q := u.Query()
	q.Set(key, value)
	q.Set("iss", s.issuer)
	if request.state != "" {
		q.Set("state", request.state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	clientID, ok := s.authenticateClient(w, r, form)
	if !ok {
		return
	}
	if form.Get("resource") != s.resource {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource must identify this MCP endpoint")
		return
	}
	if requestedScope := form.Get("scope"); requestedScope != "" && requestedScope != scope {
		oauthError(w, http.StatusBadRequest, "invalid_scope", "only the mcp scope is supported")
		return
	}
	s.mu.Lock()
	now := s.now()
	s.cleanup(now)
	switch form.Get("grant_type") {
	case "authorization_code":
		key := digest(form.Get("code"))
		code, exists := s.codes[key]
		verifier := form.Get("code_verifier")
		challenge := sha256.Sum256([]byte(verifier))
		if !exists || !now.Before(code.expires) || code.clientID != clientID || code.redirectURI != form.Get("redirect_uri") || code.resource != form.Get("resource") || !verifierPattern.MatchString(verifier) || code.challenge != base64.RawURLEncoding.EncodeToString(challenge[:]) {
			s.mu.Unlock()
			oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code or proof is invalid")
			return
		}
		if code.consumed {
			s.revokeFamily(code.family)
			s.mu.Unlock()
			oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code was already used")
			return
		}
		if len(s.families) >= familyLimit || len(s.refresh) >= refreshLimit {
			s.mu.Unlock()
			oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many active grants")
			return
		}
		familyToken, err := randomToken("")
		if err != nil {
			s.mu.Unlock()
			oauthError(w, http.StatusInternalServerError, "server_error", "could not create grant")
			return
		}
		family := digest(familyToken)
		access, refresh, err := s.issueTokens(family, now)
		if err != nil {
			s.mu.Unlock()
			oauthError(w, http.StatusInternalServerError, "server_error", "could not create tokens")
			return
		}
		s.families[family] = grantFamily{clientID: clientID, expires: now.Add(refreshLifetime)}
		code.consumed, code.family = true, family
		s.codes[key] = code
		s.mu.Unlock()
		writeTokens(w, access, refresh)
	case "refresh_token":
		key := digest(form.Get("refresh_token"))
		refresh, exists := s.refresh[key]
		family, familyExists := s.families[refresh.family]
		if !exists || !familyExists || !now.Before(family.expires) || family.clientID != clientID {
			s.mu.Unlock()
			oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
			return
		}
		if refresh.used {
			s.revokeFamily(refresh.family)
			s.mu.Unlock()
			oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token was already used")
			return
		}
		if len(s.refresh) >= refreshLimit {
			s.mu.Unlock()
			oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many active refresh tokens")
			return
		}
		access, nextRefresh, err := s.issueTokens(refresh.family, now)
		if err != nil {
			s.mu.Unlock()
			oauthError(w, http.StatusInternalServerError, "server_error", "could not create tokens")
			return
		}
		refresh.used = true
		s.refresh[key] = refresh
		s.mu.Unlock()
		writeTokens(w, access, nextRefresh)
	default:
		s.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (s *Server) issueTokens(family [32]byte, now time.Time) (string, string, error) {
	access, err := randomToken("lrmcp_at_")
	if err != nil {
		return "", "", err
	}
	refresh, err := randomToken("lrmcp_rt_")
	if err != nil {
		return "", "", err
	}
	s.access[digest(access)] = accessGrant{family: family, expires: now.Add(accessLifetime)}
	s.refresh[digest(refresh)] = refreshGrant{family: family}
	return access, refresh, nil
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	clientID, ok := s.authenticateClient(w, r, form)
	if !ok {
		return
	}
	s.mu.Lock()
	s.cleanup(s.now())
	key := digest(form.Get("token"))
	familyID := [32]byte{}
	if refresh, exists := s.refresh[key]; exists {
		familyID = refresh.family
	} else if access, exists := s.access[key]; exists {
		familyID = access.family
	}
	if family, exists := s.families[familyID]; exists && family.clientID == clientID {
		s.revokeFamily(familyID)
	}
	s.mu.Unlock()
	// RFC 7009 deliberately does not reveal whether the token existed.
	w.WriteHeader(http.StatusOK)
}

func (s *Server) revokeFamily(id [32]byte) {
	delete(s.families, id)
	for token, grant := range s.access {
		if grant.family == id {
			delete(s.access, token)
		}
	}
	for token, grant := range s.refresh {
		if grant.family == id {
			delete(s.refresh, token)
		}
	}
}

func (s *Server) authenticateClient(w http.ResponseWriter, r *http.Request, form url.Values) (string, bool) {
	clientID, secret := form.Get("client_id"), form.Get("client_secret")
	if r.Header.Get("Authorization") != "" {
		basicID, basicSecret, ok := r.BasicAuth()
		if !ok || form.Has("client_secret") {
			w.Header().Set("WWW-Authenticate", `Basic realm="Local Runtime MCP OAuth"`)
			oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return "", false
		}
		var idErr, secretErr error
		basicID, idErr = url.QueryUnescape(basicID)
		basicSecret, secretErr = url.QueryUnescape(basicSecret)
		if idErr != nil || secretErr != nil || (clientID != "" && clientID != basicID) {
			oauthError(w, http.StatusBadRequest, "invalid_request", "conflicting client authentication")
			return "", false
		}
		clientID, secret = basicID, basicSecret
	}
	registered, exists := s.clients[clientID]
	secretHash := digest(secret)
	if subtle.ConstantTimeCompare(registered.secret[:], secretHash[:]) != 1 || !exists {
		w.Header().Set("WWW-Authenticate", `Basic realm="Local Runtime MCP OAuth"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return "", false
	}
	return clientID, true
}

func (s *Server) cleanup(now time.Time) {
	for key, ticket := range s.tickets {
		if !now.Before(ticket.expires) {
			delete(s.tickets, key)
		}
	}
	for key, code := range s.codes {
		if !now.Before(code.expires) {
			delete(s.codes, key)
		}
	}
	for key, family := range s.families {
		if !now.Before(family.expires) {
			delete(s.families, key)
		}
	}
	for key, grant := range s.access {
		_, exists := s.families[grant.family]
		if !now.Before(grant.expires) || !exists {
			delete(s.access, key)
		}
	}
	for key, grant := range s.refresh {
		if _, exists := s.families[grant.family]; !exists {
			delete(s.refresh, key)
		}
	}
}

func validChallenge(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func digest(value string) [32]byte { return sha256.Sum256([]byte(value)) }

func randomToken(prefix string) (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create random token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random[:]), nil
}

func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method == allowed {
		return true
	}
	w.Header().Set("Allow", allowed)
	oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "unsupported method")
	return false
}

func readForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "send application/x-www-form-urlencoded")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, formLimit)
	if err := r.ParseForm(); err != nil || !singleValues(r.PostForm) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid or oversized form")
		return nil, false
	}
	return r.PostForm, true
}

func singleValues(values url.Values) bool {
	for _, entries := range values {
		if len(entries) != 1 {
			return false
		}
	}
	return true
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func writeTokens(w http.ResponseWriter, access, refresh string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(accessLifetime.Seconds()),
		"refresh_token": refresh, "scope": scope,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
