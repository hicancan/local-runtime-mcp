package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func nativeCapabilityGrant(cfg *config.Config) (*config.WebGrant, error) {
	native := cfg.Capabilities.Native
	if native == nil {
		return nil, nil
	}
	if len(native.Key) < 32 || strings.ContainsAny(native.Key, " \t\r\n") {
		return nil, errors.New("the native capability key must contain at least 32 characters without whitespace")
	}
	for _, key := range []string{cfg.Models.Key, cfg.Browser.Token, cfg.HTTP.BearerToken, cfg.HTTP.OAuth.OwnerToken, cfg.OpenAI.APIKey, cfg.Cloudflare.TunnelToken} {
		if native.Key == key {
			return nil, errors.New("the native capability key must be independent of other runtime credentials")
		}
	}
	for _, client := range cfg.HTTP.OAuth.Clients {
		if native.Key == client.Secret {
			return nil, errors.New("the native capability key must be independent of other runtime credentials")
		}
	}
	for _, grant := range cfg.Capabilities.Web {
		if grant.Site == native.Site && len(grant.Capabilities) > 0 {
			return &grant, nil
		}
	}
	return nil, errors.New("native capabilities require a configured web capability grant for their site")
}

func nativeCapabilityHandler(grant config.WebGrant, ownerBrowser string, registry *capability.Registry, status func() browser.Status) http.Handler {
	definitions := make([]capability.Definition, 0)
	targets := make(map[string]string)
	for _, definition := range registry.List() {
		target, granted := grant.Capabilities[definition.Name]
		if granted && definition.Annotations.ReadOnlyHint && !definition.Annotations.DestructiveHint {
			definitions = append(definitions, definition)
			targets[definition.Name] = target
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /capabilities", func(w http.ResponseWriter, _ *http.Request) {
		writeNativeCapabilityJSON(w, http.StatusOK, definitions)
	})
	mux.HandleFunc("POST /capabilities/invoke", func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeNativeCapabilityError(w, http.StatusUnsupportedMediaType, "invalid_request", "Capability requests require application/json.")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		var request capabilityRequest
		if err != nil || r.URL.RawQuery != "" || decodeWeb(data, &request) != nil || request.Name == "" || request.Input == nil {
			writeNativeCapabilityError(w, http.StatusBadRequest, "invalid_request", "Send a capability name and an input object.")
			return
		}
		target, granted := targets[request.Name]
		if !granted {
			writeNativeCapabilityError(w, http.StatusForbidden, "capability_not_granted", "This read-only capability is not granted.")
			return
		}
		browserID := ""
		for _, instance := range status().Instances {
			if !instance.Connected || ownerBrowser != "" && instance.BrowserID != ownerBrowser {
				continue
			}
			if browserID != "" {
				writeNativeCapabilityError(w, http.StatusConflict, "browser_ambiguous", "Choose a browser profile in the local runtime configuration.")
				return
			}
			browserID = instance.BrowserID
		}
		if browserID == "" {
			writeNativeCapabilityError(w, http.StatusServiceUnavailable, "browser_unavailable", "Connect the configured browser profile.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		result, err := registry.CallBound(ctx, request.Name, browserID, target, request.Input)
		switch {
		case err == nil:
			writeNativeCapabilityJSON(w, http.StatusOK, result)
		case errors.Is(err, capability.ErrInvalidInput):
			writeNativeCapabilityError(w, http.StatusBadRequest, "invalid_input", "The input does not match this capability's schema.")
		case errors.Is(err, capability.ErrTargetMissing):
			writeNativeCapabilityError(w, http.StatusConflict, "target_unavailable", "Open the configured page in the selected browser profile.")
		case errors.Is(err, capability.ErrTargetAmbiguous):
			writeNativeCapabilityError(w, http.StatusConflict, "target_ambiguous", "Leave only one matching page in the selected browser profile.")
		case errors.Is(err, capability.ErrBrowserTabs):
			writeNativeCapabilityError(w, http.StatusServiceUnavailable, "browser_unavailable", "Connect the configured browser profile.")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			writeNativeCapabilityError(w, http.StatusGatewayTimeout, "capability_timeout", "The capability did not finish in time.")
		default:
			writeNativeCapabilityError(w, http.StatusBadGateway, "capability_failed", "The capability could not read the configured page.")
		}
	})
	return mux
}

func writeNativeCapabilityError(w http.ResponseWriter, status int, code, message string) {
	writeNativeCapabilityJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeNativeCapabilityJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
