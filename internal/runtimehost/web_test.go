package runtimehost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
)

type unavailableTabs struct{ calls int }

func (t *unavailableTabs) Tabs(context.Context, string) ([]browser.Tab, error) {
	t.calls++
	return nil, nil
}

func (t *unavailableTabs) Act(context.Context, browser.Action) (browser.ActionResult, error) {
	panic("unexpected browser action")
}

func TestWebOriginAndCapabilityGrants(t *testing.T) {
	tabs := &unavailableTabs{}
	registry, err := capability.Load("", tabs)
	if err != nil {
		t.Fatal(err)
	}
	handler := webHandler(newGrantStore([]config.WebGrant{{Site: "https://studev.example", Use: true}}, config.RemoteGrants{}, "", registry, nil, nil, nil, nil), registry, nil)
	source := browser.WebSource{Origin: "https://evil.example", BrowserID: "browser-a"}
	emit := func(browser.WebEvent) error { t.Fatal("unauthorized request emitted an event"); return nil }
	for _, method := range []string{"capabilities.list", "capabilities.invoke", "models.list", "models.chat"} {
		if _, err := handler(context.Background(), source, browser.WebRequest{Method: method, Params: json.RawMessage(`{}`)}, emit); err == nil {
			t.Fatal("foreign origin permitted", method)
		}
	}
	source.Origin = "https://studev.example"
	if _, err := handler(context.Background(), source, browser.WebRequest{Method: "models.list", Params: json.RawMessage(`{}`)}, emit); err == nil {
		t.Fatal("removed models.list method was permitted")
	}
	if _, err := handler(context.Background(), source, browser.WebRequest{Method: "capabilities.invoke", Params: json.RawMessage(`{"name":"deepseek.read","input":{},"tab_id":"forged"}`)}, emit); err == nil {
		t.Fatal("page-selected target accepted")
	}
	if tabs.calls != 0 {
		t.Fatal("unauthorized request reached browser")
	}
	value, err := handler(context.Background(), source, browser.WebRequest{Method: "capabilities.list", Params: json.RawMessage(`{}`)}, emit)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if string(encoded) != `[]` {
		t.Fatalf("unexpected capabilities %s", encoded)
	}
}

func TestValidateExactOriginsAndCredentialSeparation(t *testing.T) {
	for _, origin := range []string{"https://example.com/", "https://example.com/path", "https://example.com#fragment", "https://user@example.com", "https://*.example.com"} {
		cfg := config.Default()
		cfg.Grants.Web = []config.WebGrant{{Site: origin}}
		if err := validateGrantsAndCredentials(cfg); err == nil {
			t.Fatalf("accepted non-exact origin %s", origin)
		}
	}
	cfg := config.Default()
	shared := strings.Repeat("x", 32)
	cfg.Grants.Clients = []config.ClientGrant{{ID: "studev-win", Token: shared}}
	cfg.HTTP.BearerToken = shared
	if validateGrantsAndCredentials(cfg) == nil {
		t.Fatal("machine credential accepted as client credential")
	}
	cfg.HTTP.BearerToken = ""
	cfg.Local.Token = shared
	if validateGrantsAndCredentials(cfg) == nil {
		t.Fatal("local owner token accepted as client credential")
	}
	cfg.Local.Token = strings.Repeat("y", 32)
	cfg.Grants.Web = []config.WebGrant{{Site: "http://127.0.0.1:4321"}}
	if err := validateGrantsAndCredentials(cfg); err != nil {
		t.Fatal(err)
	}
}
