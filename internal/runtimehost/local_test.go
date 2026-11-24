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

type localCourseBrowser struct {
	tabs    []browser.Tab
	asked   string
	actions []browser.Action
}

func (b *localCourseBrowser) Tabs(_ context.Context, browserID string) ([]browser.Tab, error) {
	b.asked = browserID
	return b.tabs, nil
}

func (b *localCourseBrowser) Act(_ context.Context, action browser.Action) (browser.ActionResult, error) {
	b.actions = append(b.actions, action)
	return browser.ActionResult{Success: true, Value: map[string]any{
		"course":   map[string]any{"title": "Example course", "url": "https://www.icourse163.org/course/example"},
		"chapters": []any{map[string]any{"title": "First chapter", "itemsLoaded": false, "items": []any{}}},
	}}, nil
}

func TestLocalClientCapabilitiesAreScopedPerClientWithoutBorrowingWebGrant(t *testing.T) {
	peer := &localCourseBrowser{tabs: []browser.Tab{{ID: "a", BrowserID: "owner", URL: testCourse}}}
	registry, err := capability.Load(filepath.Join("..", "..", "capabilities"), peer)
	if err != nil {
		t.Fatal(err)
	}
	studevToken := strings.Repeat("s", 32)
	otherToken := strings.Repeat("h", 32)
	clients := []config.ClientGrant{
		{
			ID:    "studev-win",
			Token: studevToken,
			Capabilities: map[string]string{
				"icourse.outline": testCourse,
				"deepseek.start":  "https://chat.deepseek.com/",
			},
		},
		{
			ID:    "reader-only",
			Token: otherToken,
		},
	}
	chosen := func() browser.Status {
		return browser.Status{Instances: []browser.InstanceStatus{{BrowserID: "owner", Connected: true}}}
	}
	handler := localCapabilitiesHandler(clients, registry, chosen)
	request := func(handler http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	// Second client has no capabilities granted.
	otherList := request(handler, otherToken, http.MethodGet, "/capabilities", "")
	if otherList.Code != http.StatusOK || strings.TrimSpace(otherList.Body.String()) != "[]" {
		t.Fatalf("unexpected empty client capability list: %d %s", otherList.Code, otherList.Body.String())
	}

	// Unauthenticated or wrong token is rejected.
	if unauth := request(handler, strings.Repeat("z", 32), http.MethodGet, "/capabilities", ""); unauth.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token accepted: %d", unauth.Code)
	}

	// StuDev Windows sees only its granted read-only capability (deepseek.start is mutating so excluded).
	listed := request(handler, studevToken, http.MethodGet, "/capabilities", "")
	var definitions []capability.Definition
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &definitions) != nil || len(definitions) != 1 || definitions[0].Name != "icourse.outline" || definitions[0].Title != "课程目录（中国大学 MOOC）" {
		t.Fatalf("unexpected scoped catalog %s", listed.Body)
	}

	const invoke = "/capabilities/invoke"
	const course = `{"name":"icourse.outline","input":{}}`
	for _, test := range []struct{ name, token, path, body, code string }{
		{"mutating registered capability", studevToken, invoke, `{"name":"deepseek.start","input":{}}`, "capability_not_granted"},
		{"ungranted read-only capability", studevToken, invoke, `{"name":"deepseek.read","input":{}}`, "capability_not_granted"},
		{"other client ungranted capability", otherToken, invoke, course, "capability_not_granted"},
		{"machine tool", studevToken, invoke, `{"name":"process_run","input":{}}`, "capability_not_granted"},
		{"direct browser", studevToken, invoke, `{"name":"icourse.outline","input":{},"browser_id":"other"}`, "invalid_request"},
		{"direct target", studevToken, invoke, `{"name":"icourse.outline","input":{},"url":"https://elsewhere.example"}`, "invalid_request"},
		{"target query", studevToken, invoke + "?tab_id=other", course, "invalid_request"},
		{"bad schema", studevToken, invoke, `{"name":"icourse.outline","input":{"url":"https://elsewhere.example"}}`, "invalid_input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := request(handler, test.token, http.MethodPost, test.path, test.body)
			if result.Code < 400 || !strings.Contains(result.Body.String(), `"code":"`+test.code+`"`) || len(peer.actions) != 0 {
				t.Fatalf("request reached browser or wrong response: %d %s", result.Code, result.Body)
			}
		})
	}

	result := request(handler, studevToken, http.MethodPost, invoke, course)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"itemsLoaded":false`) || peer.asked != "owner" || len(peer.actions) != 1 || peer.actions[0].TabID != "a" {
		t.Fatalf("exact allowed page was not read once: %d %s", result.Code, result.Body)
	}

	// Browser profile ambiguity & selection
	states := []browser.InstanceStatus{{BrowserID: "unknown", Connected: true}, {BrowserID: "owner", Connected: true}}
	status := func() browser.Status { return browser.Status{Instances: states} }
	for _, test := range []struct{ owner, code string }{{"", "browser_ambiguous"}, {"missing", "browser_unavailable"}} {
		c := []config.ClientGrant{{ID: "studev-win", Token: studevToken, BrowserID: test.owner, Capabilities: map[string]string{"icourse.outline": testCourse}}}
		result := request(localCapabilitiesHandler(c, registry, status), studevToken, http.MethodPost, invoke, course)
		if result.Code < 400 || !strings.Contains(result.Body.String(), test.code) || len(peer.actions) != 1 {
			t.Fatalf("unexpected profile handling %d %s", result.Code, result.Body)
		}
	}

	peer.tabs = append(peer.tabs, browser.Tab{ID: "b", BrowserID: "owner", URL: testCourse})
	c := []config.ClientGrant{{ID: "studev-win", Token: studevToken, BrowserID: "owner", Capabilities: map[string]string{"icourse.outline": testCourse}}}
	result = request(localCapabilitiesHandler(c, registry, status), studevToken, http.MethodPost, invoke, course)
	if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "target_ambiguous") || len(peer.actions) != 1 {
		t.Fatalf("duplicate target must not execute: %d %s", result.Code, result.Body)
	}
	peer.tabs = []browser.Tab{{ID: "a", BrowserID: "owner", URL: testCourse + "/different"}}
	result = request(localCapabilitiesHandler(c, registry, status), studevToken, http.MethodPost, invoke, course)
	if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "target_unavailable") || len(peer.actions) != 1 {
		t.Fatalf("different same-site record must not execute: %d %s", result.Code, result.Body)
	}
}

func TestValidateGrantsRejectsDuplicateTokensAndInvalidClientIDs(t *testing.T) {
	cfg := config.Default()
	tok := strings.Repeat("a", 32)
	cfg.Grants.Clients = []config.ClientGrant{{ID: "Bad ID", Token: tok}}
	if err := validateGrantsAndCredentials(cfg); err == nil {
		t.Fatal("invalid client ID accepted")
	}
	cfg.Grants.Clients = []config.ClientGrant{
		{ID: "client-a", Token: tok},
		{ID: "client-b", Token: tok},
	}
	if err := validateGrantsAndCredentials(cfg); err == nil {
		t.Fatal("duplicate token across two clients accepted")
	}
	cfg.Grants.Clients = []config.ClientGrant{
		{ID: "client-a", Token: tok},
		{ID: "client-b", Token: strings.Repeat("b", 32)},
	}
	cfg.Local.Token = strings.Repeat("c", 32)
	if err := validateGrantsAndCredentials(cfg); err != nil {
		t.Fatalf("valid independent client grants rejected: %v", err)
	}
}
