package runtimehost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/modelapi"
)

type unavailableTabs struct{ calls int }

func (t *unavailableTabs) Tabs(context.Context, string) ([]browser.Tab, error) {
	t.calls++
	return nil, nil
}

func (t *unavailableTabs) Act(context.Context, browser.Action) (browser.ActionResult, error) {
	panic("unexpected browser action")
}

func TestWebOriginAndModelGrants(t *testing.T) {
	tabs := &unavailableTabs{}
	registry, err := capability.Load("", tabs)
	if err != nil {
		t.Fatal(err)
	}
	service, err := modelapi.New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := webHandler(newGrantStore([]config.WebGrant{{Site: "https://studev.example", Use: true, Models: nil}}, "", registry, nil, nil), registry, service)
	source := browser.WebSource{Origin: "https://evil.example", BrowserID: "browser-a"}
	emit := func(browser.WebEvent) error { t.Fatal("unauthorized request emitted an event"); return nil }
	for _, method := range []string{"models.list", "models.chat", "models.close", "capabilities.list", "capabilities.invoke"} {
		if _, err := handler(context.Background(), source, browser.WebRequest{Method: method, Params: json.RawMessage(`{"model":"hidden","messages":[]}`)}, emit); err == nil {
			t.Fatal("foreign origin permitted", method)
		}
	}
	source.Origin = "https://studev.example"
	if _, err := handler(context.Background(), source, browser.WebRequest{Method: "models.chat", Params: json.RawMessage(`{"model":"hidden","messages":[]}`)}, emit); err == nil {
		t.Fatal("ungranted model permitted")
	}
	if _, err := handler(context.Background(), source, browser.WebRequest{Method: "capabilities.invoke", Params: json.RawMessage(`{"name":"deepseek.read","input":{},"tab_id":"forged"}`)}, emit); err == nil {
		t.Fatal("page-selected target accepted")
	}
	if tabs.calls != 0 {
		t.Fatal("unauthorized request reached browser")
	}
	value, err := handler(context.Background(), source, browser.WebRequest{Method: "models.list", Params: json.RawMessage(`{}`)}, emit)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if string(encoded) != `{"data":[],"object":"list"}` {
		t.Fatalf("unexpected models %s", encoded)
	}
}

func TestValidateExactOriginsAndCredentialSeparation(t *testing.T) {
	for _, origin := range []string{"https://example.com/", "https://example.com/path", "https://example.com#fragment", "https://user@example.com", "https://*.example.com"} {
		cfg := config.Default()
		cfg.Capabilities.Web = []config.WebGrant{{Site: origin}}
		if err := validateWebConfig(cfg); err == nil {
			t.Fatalf("accepted non-exact origin %s", origin)
		}
	}
	cfg := config.Default()
	cfg.Models.Key = strings.Repeat("x", 32)
	cfg.HTTP.BearerToken = cfg.Models.Key
	if validateWebConfig(cfg) == nil {
		t.Fatal("machine credential accepted as model credential")
	}
	cfg.Models.Key = strings.Repeat("y", 32)
	cfg.Capabilities.Web = []config.WebGrant{{Site: "http://127.0.0.1:4321"}}
	if err := validateWebConfig(cfg); err != nil {
		t.Fatal(err)
	}
}
