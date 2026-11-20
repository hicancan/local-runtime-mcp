package runtimehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/modelapi"
)

func validateWebConfig(cfg *config.Config) error {
	if cfg.Models.Key != "" && len(cfg.Models.Key) < 32 {
		return errors.New("model key must contain at least 32 characters")
	}
	// A model-only key must not accidentally grant machine control.
	if cfg.Models.Key != "" && (cfg.Models.Key == cfg.HTTP.BearerToken || cfg.Models.Key == cfg.Browser.Token || cfg.Models.Key == cfg.HTTP.OAuth.OwnerToken) {
		return errors.New("model key must differ from machine and browser credentials")
	}
	seen := map[string]bool{}
	for _, g := range cfg.Capabilities.Web {
		if !config.ValidSite(g.Site) || seen[g.Site] {
			return errors.New("web grants require unique sites (https://example.com, or an exact local origin)")
		}
		seen[g.Site] = true
		for _, target := range g.Capabilities {
			u, err := url.Parse(target)
			if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" && u.Scheme != "http" {
				return errors.New("web capability targets must be absolute HTTP(S) URLs without credentials")
			}
		}
	}
	return nil
}

func validateCapabilities(cfg *config.Config, registry *capability.Registry) error {
	definitions := map[string]capability.Definition{}
	for _, d := range registry.List() {
		definitions[d.Name] = d
	}
	models := map[string]bool{}
	for _, m := range cfg.Models.Items {
		models[m.ID] = true
		for _, name := range []string{m.Start, m.Read, m.Stop} {
			d, ok := definitions[name]
			if !ok || !allowsTarget(d, m.URL) {
				return errors.New("model capability is missing or does not allow its target origin")
			}
		}
	}
	for _, g := range cfg.Capabilities.Web {
		for _, id := range g.Models {
			if !models[id] {
				return errors.New("web grant refers to an unregistered model")
			}
		}
		for name, target := range g.Capabilities {
			d, ok := definitions[name]
			if !ok || !allowsTarget(d, target) {
				return errors.New("web grant capability is missing or does not allow its target origin")
			}
		}
	}
	return nil
}

func allowsTarget(d capability.Definition, target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	for _, origin := range d.Origins {
		if origin == u.Scheme+"://"+u.Host {
			return true
		}
	}
	return false
}

type capabilityRequest struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func webHandler(grants *grantStore, registry *capability.Registry, models *modelapi.Service) browser.WebHandler {
	return func(ctx context.Context, source browser.WebSource, request browser.WebRequest, emit func(browser.WebEvent) error) (any, error) {
		grant, ok := grants.get(source.Origin)
		if !ok || !grant.Use {
			return nil, errors.New("this website has no local runtime grant")
		}
		switch request.Method {
		case "capabilities.list":
			result := []capability.Definition{}
			for _, d := range registry.List() {
				if _, ok := grant.Capabilities[d.Name]; ok {
					result = append(result, d)
				}
			}
			// Machine tools lent to this site come with their annotations: the site asks its person before any change.
			for _, d := range grants.machine.list() {
				if contains(grant.Tools, d.Name) {
					result = append(result, d)
				}
			}
			return result, nil
		case "capabilities.invoke":
			var p capabilityRequest
			if decodeWeb(request.Params, &p) != nil || p.Input == nil {
				return nil, errors.New("invalid capability input")
			}
			if contains(grant.Tools, p.Name) {
				return grants.machine.call(ctx, p.Name, p.Input)
			}
			target, ok := grant.Capabilities[p.Name]
			if !ok {
				return nil, errors.New("this capability is not granted to this website")
			}
			result, err := registry.CallBound(ctx, p.Name, source.BrowserID, target, p.Input)
			if err != nil {
				if errors.Is(err, capability.ErrTargetMissing) || errors.Is(err, capability.ErrTargetAmbiguous) || errors.Is(err, capability.ErrBrowserTabs) || errors.Is(err, capability.ErrInvalidInput) {
					return nil, err
				}
				return nil, errors.New("capability execution failed; no request was replayed")
			}
			return result, nil
		case "models.list":
			result := []map[string]any{}
			for _, m := range models.List() {
				if contains(grant.Models, m["id"].(string)) {
					result = append(result, m)
				}
			}
			return map[string]any{"object": "list", "data": result}, nil
		case "models.chat":
			r, err := modelapi.DecodeRequest(request.Params)
			if err != nil {
				return nil, err
			}
			if !contains(grant.Models, r.Model) {
				return nil, errors.New("this model is not granted to this website")
			}
			answer, err := models.Generate(ctx, r, source.BrowserID, source.Origin+"|"+source.BrowserID, func(d modelapi.Delta) error { return emit(browser.WebEvent{Type: "delta", Data: d}) })
			if err != nil {
				var safe *modelapi.Error
				if errors.As(err, &safe) {
					return nil, safe
				}
				if ctx.Err() != nil {
					return nil, errors.New("local model request cancelled or timed out; source stop was requested")
				}
				return nil, errors.New("local model source failed; no request was replayed")
			}
			return modelapi.Completion(r.Model, answer), nil
		case "models.close":
			var p struct {
				Model          string `json:"model"`
				ConversationID string `json:"conversation_id"`
			}
			if decodeWeb(request.Params, &p) != nil || !contains(grant.Models, p.Model) {
				return nil, errors.New("this model conversation is not granted to this website")
			}
			if err := models.CloseConversation(ctx, source.Origin+"|"+source.BrowserID, p.Model, p.ConversationID); err != nil {
				return nil, errors.New("the model conversation could not be closed")
			}
			return map[string]any{"closed": true, "conversation_id": p.ConversationID}, nil
		default:
			return nil, errors.New("unsupported local runtime operation")
		}
	}
}

func decodeWeb(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one request")
	}
	return nil
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
