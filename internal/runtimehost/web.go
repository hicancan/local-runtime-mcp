package runtimehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func validateCapabilities(cfg *config.Config, registry *capability.Registry) error {
	definitions := map[string]capability.Definition{}
	for _, d := range registry.List() {
		definitions[d.Name] = d
	}
	for _, g := range cfg.Grants.Web {
		for name, target := range g.Capabilities {
			d, ok := definitions[name]
			if !ok || !allowsTarget(d, target) {
				return errors.New("web grant capability is missing or does not allow its target origin")
			}
		}
	}
	for _, c := range cfg.Grants.Clients {
		for name, target := range c.Capabilities {
			d, ok := definitions[name]
			if !ok || !allowsTarget(d, target) {
				return errors.New("local client grant capability is missing or does not allow its target origin")
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

func webHandler(grants *grantStore, registry *capability.Registry, store *audit.Store) browser.WebHandler {
	return func(ctx context.Context, source browser.WebSource, request browser.WebRequest, _ func(browser.WebEvent) error) (any, error) {
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
			rawInput, _ := json.Marshal(p.Input)
			start := time.Now()
			if contains(grant.Tools, p.Name) {
				evID, sid := store.StartTool(audit.StartToolInput{
					SessionID:  "web:" + source.Origin,
					AgentID:    source.Origin,
					ClientName: source.Origin,
					Wire:       "web",
					ToolName:   p.Name,
					RawArgs:    rawInput,
				})
				res, err := grants.machine.call(ctx, p.Name, p.Input)
				dur := time.Since(start).Milliseconds()
				if err != nil {
					store.FinishTool(evID, sid, audit.FinishToolInput{Status: "error", ErrorText: err.Error(), DurationMS: dur})
				} else {
					store.FinishTool(evID, sid, audit.FinishToolInput{Status: "ok", Result: res, DurationMS: dur})
				}
				return res, err
			}
			target, ok := grant.Capabilities[p.Name]
			if !ok {
				return nil, errors.New("this capability is not granted to this website")
			}
			evID, sid := store.StartTool(audit.StartToolInput{
				SessionID:  "web:" + source.Origin,
				AgentID:    source.Origin,
				ClientName: source.Origin,
				Wire:       "web",
				ToolName:   p.Name,
				RawArgs:    rawInput,
			})
			result, err := registry.CallBound(ctx, p.Name, source.BrowserID, target, p.Input)
			dur := time.Since(start).Milliseconds()
			if err != nil {
				store.FinishTool(evID, sid, audit.FinishToolInput{Status: "error", ErrorText: err.Error(), DurationMS: dur})
				if errors.Is(err, capability.ErrTargetMissing) || errors.Is(err, capability.ErrTargetAmbiguous) || errors.Is(err, capability.ErrBrowserTabs) || errors.Is(err, capability.ErrInvalidInput) {
					return nil, err
				}
				return nil, errors.New("capability execution failed; no request was replayed")
			}
			store.FinishTool(evID, sid, audit.FinishToolInput{Status: "ok", Result: result, DurationMS: dur})
			return result, nil
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
