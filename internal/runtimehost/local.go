package runtimehost

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func validateGrantsAndCredentials(cfg *config.Config) error {
	seenSites := map[string]bool{}
	for _, g := range cfg.Grants.Web {
		if !config.ValidSite(g.Site) || seenSites[g.Site] {
			return errors.New("web grants require unique sites (https://example.com, or an exact local origin)")
		}
		seenSites[g.Site] = true
		for _, target := range g.Capabilities {
			if !validTargetURL(target) {
				return errors.New("web capability targets must be absolute HTTP(S) URLs without credentials")
			}
		}
	}

	seenClients := map[string]bool{}
	for _, c := range cfg.Grants.Clients {
		if !config.ValidClientID(c.ID) || seenClients[c.ID] {
			return fmt.Errorf("local client grants require unique lowercase identifiers: %q", c.ID)
		}
		seenClients[c.ID] = true
		if len(c.Token) < 32 || strings.ContainsAny(c.Token, " \t\r\n") {
			return fmt.Errorf("local client %q token must contain at least 32 characters without whitespace", c.ID)
		}
		for _, target := range c.Capabilities {
			if !validTargetURL(target) {
				return fmt.Errorf("local client %q capability targets must be absolute HTTP(S) URLs without credentials", c.ID)
			}
		}
	}

	// Every configured credential across every plane and client must be distinct.
	credentials := map[string]string{}
	claim := func(role, value string) error {
		if value == "" {
			return nil
		}
		if existing, duplicate := credentials[value]; duplicate {
			return fmt.Errorf("credential for %s must be independent of %s", role, existing)
		}
		credentials[value] = role
		return nil
	}
	for _, pair := range [][2]string{
		{"browser.token", cfg.Browser.Token},
		{"local.token", cfg.Local.Token},
		{"http.bearer_token", cfg.HTTP.BearerToken},
		{"http.oauth.owner_token", cfg.HTTP.OAuth.OwnerToken},
		{"openai.api_key", cfg.OpenAI.APIKey},
		{"cloudflare.tunnel_token", cfg.Cloudflare.TunnelToken},
	} {
		if err := claim(pair[0], pair[1]); err != nil {
			return err
		}
	}
	for _, client := range cfg.HTTP.OAuth.Clients {
		if err := claim("http.oauth.clients."+client.ID, client.Secret); err != nil {
			return err
		}
	}
	for _, client := range cfg.Grants.Clients {
		if err := claim("grants.clients."+client.ID, client.Token); err != nil {
			return err
		}
	}
	return nil
}

func validTargetURL(target string) bool {
	u, err := url.Parse(target)
	return err == nil && u.Host != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http")
}

func findClientByToken(clients []config.ClientGrant, bearer string) (config.ClientGrant, bool) {
	if bearer == "" {
		return config.ClientGrant{}, false
	}
	var matched config.ClientGrant
	found := false
	for _, client := range clients {
		if len(client.Token) == len(bearer) && subtle.ConstantTimeCompare([]byte(client.Token), []byte(bearer)) == 1 {
			matched = client
			found = true
		}
	}
	return matched, found
}

func localCapabilitiesHandler(clients []config.ClientGrant, registry *capability.Registry, status func() browser.Status) http.Handler {
	copied := append([]config.ClientGrant(nil), clients...)
	authenticate := func(w http.ResponseWriter, r *http.Request) (config.ClientGrant, bool) {
		provided, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		client, ok := findClientByToken(copied, provided)
		if !bearer || !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-capabilities"`)
			writeLocalCapabilityError(w, http.StatusUnauthorized, "unauthorized", "A valid local client token is required.")
			return config.ClientGrant{}, false
		}
		return client, true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /capabilities", func(w http.ResponseWriter, r *http.Request) {
		client, ok := authenticate(w, r)
		if !ok {
			return
		}
		definitions := make([]capability.Definition, 0)
		for _, definition := range registry.List() {
			if _, granted := client.Capabilities[definition.Name]; granted && definition.Annotations.ReadOnlyHint && !definition.Annotations.DestructiveHint {
				definitions = append(definitions, definition)
			}
		}
		writeLocalCapabilityJSON(w, http.StatusOK, definitions)
	})
	mux.HandleFunc("POST /capabilities/invoke", func(w http.ResponseWriter, r *http.Request) {
		client, ok := authenticate(w, r)
		if !ok {
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeLocalCapabilityError(w, http.StatusUnsupportedMediaType, "invalid_request", "Capability requests require application/json.")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		var request capabilityRequest
		if err != nil || r.URL.RawQuery != "" || decodeWeb(data, &request) != nil || request.Name == "" || request.Input == nil {
			writeLocalCapabilityError(w, http.StatusBadRequest, "invalid_request", "Send a capability name and an input object.")
			return
		}
		target, granted := client.Capabilities[request.Name]
		if !granted || !isReadOnlyBrowserCapability(registry, request.Name) {
			writeLocalCapabilityError(w, http.StatusForbidden, "capability_not_granted", "This read-only capability is not granted.")
			return
		}
		browserID := ""
		for _, instance := range status().Instances {
			if !instance.Connected || client.BrowserID != "" && instance.BrowserID != client.BrowserID {
				continue
			}
			if browserID != "" {
				writeLocalCapabilityError(w, http.StatusConflict, "browser_ambiguous", "Choose a browser profile in the local runtime configuration.")
				return
			}
			browserID = instance.BrowserID
		}
		if browserID == "" {
			writeLocalCapabilityError(w, http.StatusServiceUnavailable, "browser_unavailable", "Connect the configured browser profile.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		result, err := registry.CallBound(ctx, request.Name, browserID, target, request.Input)
		switch {
		case err == nil:
			writeLocalCapabilityJSON(w, http.StatusOK, result)
		case errors.Is(err, capability.ErrInvalidInput):
			writeLocalCapabilityError(w, http.StatusBadRequest, "invalid_input", "The input does not match this capability's schema.")
		case errors.Is(err, capability.ErrTargetMissing):
			writeLocalCapabilityError(w, http.StatusConflict, "target_unavailable", "Open the configured page in the selected browser profile.")
		case errors.Is(err, capability.ErrTargetAmbiguous):
			writeLocalCapabilityError(w, http.StatusConflict, "target_ambiguous", "Leave only one matching page in the selected browser profile.")
		case errors.Is(err, capability.ErrBrowserTabs):
			writeLocalCapabilityError(w, http.StatusServiceUnavailable, "browser_unavailable", "Connect the configured browser profile.")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			writeLocalCapabilityError(w, http.StatusGatewayTimeout, "capability_timeout", "The capability did not finish in time.")
		default:
			writeLocalCapabilityError(w, http.StatusBadGateway, "capability_failed", "The capability could not read the configured page.")
		}
	})
	return mux
}

func isReadOnlyBrowserCapability(registry *capability.Registry, name string) bool {
	for _, d := range registry.List() {
		if d.Name == name {
			return d.Annotations.ReadOnlyHint && !d.Annotations.DestructiveHint
		}
	}
	return false
}

func writeLocalCapabilityError(w http.ResponseWriter, status int, code, message string) {
	writeLocalCapabilityJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeLocalCapabilityJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
