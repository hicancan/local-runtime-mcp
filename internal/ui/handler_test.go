package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/audit"
)

func TestStudioSingleUseBootstrapCookieCSRFAndAuditAPI(t *testing.T) {
	store, err := audit.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, _ = store.RecordNote(audit.NoteInput{
		SessionID: "cloudflare:astra-1",
		AgentID:   "astra-1",
		Wire:      "cloudflare",
		Title:     "MOOC 任务",
		Message:   "已核对本周课程测验截止时间",
		Status:    "working",
	})

	stoppedDesktop := false
	studio, err := New(Backend{
		Audit: store,
		Status: func(context.Context) StatusSnapshot {
			st, _ := store.Stats()
			return StatusSnapshot{Version: "17.0.0", AuditStats: st}
		},
		StopDesktop: func() { stoppedDesktop = true },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Unauthenticated access to /ui/ and /ui/api/sessions is denied.
	for _, path := range []string{"/ui/", "/ui/api/sessions"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9317"+path, nil)
		studio.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without cookie: got %d", path, rec.Code)
		}
	}

	// Mint single-use bootstrap URL.
	bootstrapURL, err := studio.MintBootstrapURL("127.0.0.1:9317")
	if err != nil || !strings.Contains(bootstrapURL, "/ui/auth?code=") {
		t.Fatalf("unexpected bootstrap URL: %q %v", bootstrapURL, err)
	}

	// First visit succeeds, sets HttpOnly SameSite=Strict cookie, redirects to /ui/.
	rec := httptest.NewRecorder()
	studio.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, bootstrapURL, nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("bootstrap failed: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected cookie: %+v", cookies)
	}
	sessionCookie := cookies[0]

	// Replay of the exact same bootstrap code MUST fail (single-use).
	replay := httptest.NewRecorder()
	studio.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, bootstrapURL, nil))
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed bootstrap code was accepted: %d", replay.Code)
	}

	// Public brand favicons are served on loopback even before cookie bootstrap.
	for _, iconPath := range []string{"/ui/favicon.svg", "/favicon.svg", "/favicon.ico"} {
		iconRec := httptest.NewRecorder()
		iconReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9317"+iconPath, nil)
		studio.ServeHTTP(iconRec, iconReq)
		if iconRec.Code != http.StatusOK || !strings.Contains(iconRec.Header().Get("Content-Type"), "image/svg+xml") || !strings.Contains(iconRec.Body.String(), "<svg") {
			t.Fatalf("%s failed: code=%d ct=%q", iconPath, iconRec.Code, iconRec.Header().Get("Content-Type"))
		}
	}

	// Authenticated GET /ui/ returns HTML with strict CSP and unified favicon/logo.
	htmlRec := httptest.NewRecorder()
	htmlReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9317/ui/", nil)
	htmlReq.AddCookie(sessionCookie)
	studio.ServeHTTP(htmlRec, htmlReq)
	if htmlRec.Code != http.StatusOK || !strings.Contains(htmlRec.Body.String(), "lrmcp") || !strings.Contains(htmlRec.Body.String(), "/ui/favicon.svg") || htmlRec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("GET /ui/ failed: %d", htmlRec.Code)
	}

	// Authenticated FTS5 search API works.
	searchRec := httptest.NewRecorder()
	searchReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9317/ui/api/search?q=%E6%88%AA%E6%AD%A2%E6%97%B6%E9%97%B4", nil)
	searchReq.AddCookie(sessionCookie)
	studio.ServeHTTP(searchRec, searchReq)
	var searchResp struct {
		Events []audit.Event `json:"events"`
	}
	if searchRec.Code != http.StatusOK || json.Unmarshal(searchRec.Body.Bytes(), &searchResp) != nil || len(searchResp.Events) != 1 {
		t.Fatalf("search API failed: %d %s", searchRec.Code, searchRec.Body.String())
	}

	// State-changing POST without X-LRMCP-UI header is rejected (CSRF protection).
	csrfRec := httptest.NewRecorder()
	csrfReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9317/ui/api/desktop/stop", strings.NewReader("{}"))
	csrfReq.AddCookie(sessionCookie)
	studio.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden || stoppedDesktop {
		t.Fatalf("POST without CSRF header was not rejected: %d", csrfRec.Code)
	}

	// Cross-origin POST even WITH X-LRMCP-UI is rejected.
	crossRec := httptest.NewRecorder()
	crossReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9317/ui/api/desktop/stop", strings.NewReader("{}"))
	crossReq.AddCookie(sessionCookie)
	crossReq.Header.Set("X-LRMCP-UI", "1")
	crossReq.Header.Set("Origin", "https://evil.example")
	studio.ServeHTTP(crossRec, crossReq)
	if crossRec.Code != http.StatusForbidden || stoppedDesktop {
		t.Fatalf("cross-origin POST was not rejected: %d", crossRec.Code)
	}

	// Same-origin POST with cookie + X-LRMCP-UI succeeds.
	okRec := httptest.NewRecorder()
	okReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9317/ui/api/desktop/stop", strings.NewReader("{}"))
	okReq.AddCookie(sessionCookie)
	okReq.Header.Set("X-LRMCP-UI", "1")
	okReq.Header.Set("Origin", "http://127.0.0.1:9317")
	studio.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusOK || !stoppedDesktop {
		t.Fatalf("same-origin POST failed: %d stopped=%t", okRec.Code, stoppedDesktop)
	}
}
