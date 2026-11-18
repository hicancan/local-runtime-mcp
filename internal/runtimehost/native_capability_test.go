package runtimehost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/config"
)

const testCourse = "https://www.icourse163.org/learn/example#/learn/content"

type nativeCourseBrowser struct {
	tabs    []browser.Tab
	asked   string
	actions []browser.Action
}

func (b *nativeCourseBrowser) Tabs(_ context.Context, browserID string) ([]browser.Tab, error) {
	b.asked = browserID
	return b.tabs, nil
}

func (b *nativeCourseBrowser) Act(_ context.Context, action browser.Action) (browser.ActionResult, error) {
	b.actions = append(b.actions, action)
	return browser.ActionResult{Success: true, Value: map[string]any{
		"course":   map[string]any{"title": "Example course", "url": "https://www.icourse163.org/course/example"},
		"chapters": []any{map[string]any{"title": "First chapter", "itemsLoaded": false, "items": []any{}}},
	}}, nil
}

func TestNativeCapabilitiesExposeOnlyScopedReadOnlyBrowserCalls(t *testing.T) {
	peer := &nativeCourseBrowser{tabs: []browser.Tab{{ID: "a", BrowserID: "owner", URL: testCourse}}}
	registry, err := capability.Load(filepath.Join("..", "..", "capabilities"), peer)
	if err != nil {
		t.Fatal(err)
	}
	grant := config.WebGrant{Origin: "https://studev.example", Capabilities: map[string]string{
		"icourse.outline": testCourse,
		"deepseek.start":  "https://chat.deepseek.com/",
	}}
	chosen := func() browser.Status {
		return browser.Status{Instances: []browser.InstanceStatus{{BrowserID: "owner", Connected: true}}}
	}
	handler := nativeCapabilityHandler(grant, "", registry, chosen)
	request := func(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	listed := request(handler, http.MethodGet, "/capabilities", "")
	var definitions []capability.Definition
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &definitions) != nil || len(definitions) != 1 || definitions[0].Name != "icourse.outline" || definitions[0].Title != "课程目录（中国大学 MOOC）" {
		t.Fatalf("unexpected scoped catalog %s", listed.Body)
	}
	const invoke = "/capabilities/invoke"
	const course = `{"name":"icourse.outline","input":{}}`
	for _, test := range []struct{ name, path, body, code string }{
		{"mutating registered capability", invoke, `{"name":"deepseek.start","input":{}}`, "capability_not_granted"},
		{"ungranted read-only capability", invoke, `{"name":"deepseek.read","input":{}}`, "capability_not_granted"},
		{"machine tool", invoke, `{"name":"process_run","input":{}}`, "capability_not_granted"},
		{"direct browser", invoke, `{"name":"icourse.outline","input":{},"browser_id":"other"}`, "invalid_request"},
		{"direct target", invoke, `{"name":"icourse.outline","input":{},"url":"https://elsewhere.example"}`, "invalid_request"},
		{"target query", invoke + "?tab_id=other", course, "invalid_request"},
		{"bad schema", invoke, `{"name":"icourse.outline","input":{"url":"https://elsewhere.example"}}`, "invalid_input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := request(handler, http.MethodPost, test.path, test.body)
			if result.Code < 400 || !strings.Contains(result.Body.String(), `"code":"`+test.code+`"`) || len(peer.actions) != 0 {
				t.Fatalf("request reached browser or wrong response: %d %s", result.Code, result.Body)
			}
		})
	}
	result := request(handler, http.MethodPost, invoke, course)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"itemsLoaded":false`) || peer.asked != "owner" || len(peer.actions) != 1 || peer.actions[0].TabID != "a" {
		t.Fatalf("exact allowed page was not read once: %d %s", result.Code, result.Body)
	}
	for _, path := range []string{"/mcp", "/v1/models", "/v1/chat/completions", "/bridge/web", "/oauth/token"} {
		if wrong := request(handler, http.MethodPost, path, course); wrong.Code != http.StatusNotFound {
			t.Fatalf("endpoint unexpectedly mounted at %s: %d", path, wrong.Code)
		}
	}
	states := []browser.InstanceStatus{{BrowserID: "unknown", Connected: true}, {BrowserID: "owner", Connected: true}}
	status := func() browser.Status { return browser.Status{Instances: states} }
	for _, test := range []struct{ owner, code string }{{"", "browser_ambiguous"}, {"missing", "browser_unavailable"}} {
		result := request(nativeCapabilityHandler(grant, test.owner, registry, status), http.MethodPost, invoke, course)
		if result.Code < 400 || !strings.Contains(result.Body.String(), test.code) || len(peer.actions) != 1 {
			t.Fatalf("unexpected profile handling %d %s", result.Code, result.Body)
		}
	}
	peer.tabs = append(peer.tabs, browser.Tab{ID: "b", BrowserID: "owner", URL: testCourse})
	result = request(nativeCapabilityHandler(grant, "owner", registry, status), http.MethodPost, invoke, course)
	if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "target_ambiguous") || len(peer.actions) != 1 {
		t.Fatalf("duplicate target must not execute: %d %s", result.Code, result.Body)
	}
	peer.tabs = []browser.Tab{{ID: "a", BrowserID: "owner", URL: testCourse + "/different"}}
	result = request(nativeCapabilityHandler(grant, "owner", registry, status), http.MethodPost, invoke, course)
	if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "target_unavailable") || len(peer.actions) != 1 {
		t.Fatalf("different same-site record must not execute: %d %s", result.Code, result.Body)
	}
}

func TestNativeCapabilityGrantRequiresIndependentCredentialAndExistingScope(t *testing.T) {
	cfg := config.Default()
	if grant, err := nativeCapabilityGrant(cfg); err != nil || grant != nil {
		t.Fatal("native capabilities must default to disabled")
	}
	const origin = "https://studev.example"
	cfg.Capabilities.Web = []config.WebGrant{{Origin: origin, Capabilities: map[string]string{"icourse.outline": testCourse}}}
	cfg.Capabilities.Native = &config.NativeCapabilityGrant{Origin: origin, Key: strings.Repeat("x", 32)}
	if grant, err := nativeCapabilityGrant(cfg); err != nil || grant == nil || grant.Origin != origin {
		t.Fatalf("existing scoped grant not recognized: %v", err)
	}
	cfg.Capabilities.Native.Origin = "https://elsewhere.example"
	if _, err := nativeCapabilityGrant(cfg); err == nil {
		t.Fatal("unconfigured scope was accepted")
	}
	cfg.Capabilities.Native.Origin = origin
	for _, key := range []*string{&cfg.Models.Key, &cfg.Browser.Token, &cfg.HTTP.BearerToken, &cfg.HTTP.OAuth.OwnerToken} {
		*key = cfg.Capabilities.Native.Key
		if _, err := nativeCapabilityGrant(cfg); err == nil {
			t.Fatal("a credential shared with another entrance was accepted")
		}
		*key = ""
	}
}
