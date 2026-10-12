//go:build windows

package browser

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/oauth"
)

// Exercise the production consent form in an owned browser, not an HTTP client
// that stops at Location. The callback and final client page have separate
// origins, like an OAuth broker redirecting back to its application.
func TestEdgeOAuthAuthorizationRedirect(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_BROWSER_E2E") != "1" {
		t.Skip("set LOCAL_RUNTIME_MCP_BROWSER_E2E=1 to launch isolated Edge")
	}
	edge := findEdge()
	if edge == "" {
		t.Fatal("OAuth browser regression requires Microsoft Edge")
	}
	for _, scenario := range []struct {
		name, authentication string
		deny, wrongOwner     bool
	}{
		{name: "basic_callback_chain", authentication: "basic"},
		{name: "post_callback_chain", authentication: "post"},
		{name: "deny_callback_chain", deny: true},
		{name: "wrong_owner_then_approve", authentication: "basic", wrongOwner: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const owner = "browser-fixture-owner-01234567890123456789"
			const secret = "browser-fixture-secret-01234567890123456789"
			const verifier = "browser-fixture-verifier-012345678901234567890123456789"
			const state = "browser-fixture-opaque-state"
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var callbackCount atomic.Int32
			var approvalOrigin atomic.Value
			approvalOrigin.Store("")
			completed := make(chan error, 4)
			final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, "<!doctype html><title>OAuth client ready</title><h1>Callback chain complete</h1>")
			}))
			defer final.Close()
			origin := httptest.NewUnstartedServer(nil)
			issuer := "http://" + origin.Listener.Addr().String()
			callback := httptest.NewUnstartedServer(nil)
			redirect := "http://" + callback.Listener.Addr().String() + "/callback?registered=retained"
			authorization, err := oauth.New(oauth.Config{
				Issuer: issuer, OwnerToken: owner,
				Clients: []oauth.Client{{ID: "browser-fixture", Secret: secret, RedirectURIs: []string{redirect}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/oauth/authorize" {
					approvalOrigin.Store(r.Header.Get("Origin"))
				}
				authorization.ServeHTTP(w, r)
			})
			origin.Start()
			defer origin.Close()
			callback.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				callbackCount.Add(1)
				err := func() error {
					q := r.URL.Query()
					if approvalOrigin.Load().(string) != issuer {
						return fmt.Errorf("consent POST did not retain its same-origin Origin")
					}
					if r.Method != http.MethodGet || r.URL.Path != "/callback" || q.Get("state") != state || q.Get("iss") != issuer || q.Get("registered") != "retained" || r.Header.Get("Referer") != "" {
						return fmt.Errorf("callback method, path, state, issuer or registered query changed")
					}
					if scenario.deny {
						if q.Get("error") != "access_denied" || q.Get("code") != "" {
							return fmt.Errorf("denial did not return access_denied without a code")
						}
						return nil
					}
					if q.Get("error") != "" || q.Get("code") == "" {
						return fmt.Errorf("approval did not deliver an authorization code")
					}
					exchange := func(values url.Values) (string, string, error) {
						values.Set("resource", issuer+"/mcp")
						if scenario.authentication == "post" {
							values.Set("client_id", "browser-fixture")
							values.Set("client_secret", secret)
						}
						req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", strings.NewReader(values.Encode()))
						if err != nil {
							return "", "", err
						}
						req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
						if scenario.authentication == "basic" {
							req.SetBasicAuth("browser-fixture", secret)
						}
						response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
						if err != nil {
							return "", "", err
						}
						defer response.Body.Close()
						var tokens struct {
							Access  string `json:"access_token"`
							Refresh string `json:"refresh_token"`
							Type    string `json:"token_type"`
							Scope   string `json:"scope"`
						}
						if err := json.NewDecoder(response.Body).Decode(&tokens); err != nil {
							return "", "", err
						}
						if response.StatusCode != http.StatusOK || tokens.Type != "Bearer" || tokens.Scope != "mcp" || tokens.Refresh == "" || !authorization.ValidateBearer(tokens.Access) {
							return "", "", fmt.Errorf("token exchange did not issue usable tokens: status %d", response.StatusCode)
						}
						return tokens.Access, tokens.Refresh, nil
					}
					access, refresh, err := exchange(url.Values{
						"grant_type": {"authorization_code"}, "code": {q.Get("code")},
						"redirect_uri": {redirect}, "code_verifier": {verifier},
					})
					if err != nil {
						return err
					}
					newAccess, newRefresh, err := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
					if err != nil || access == newAccess || refresh == newRefresh {
						return fmt.Errorf("refresh did not rotate usable tokens: %v", err)
					}
					return nil
				}()
				completed <- err
				if err != nil {
					http.Error(w, "OAuth fixture callback failed", http.StatusBadGateway)
					return
				}
				http.Redirect(w, r, final.URL+"/ready", http.StatusSeeOther)
			})
			callback.Start()
			defer callback.Close()
			bridge, _ := testBridge(t)
			extension, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
			if err != nil {
				t.Fatal(err)
			}
			if err := ConfigureExtension(extension, bridge.Status().Address, testToken); err != nil {
				t.Fatal(err)
			}
			challenge := sha256.Sum256([]byte(verifier))
			params := url.Values{
				"response_type": {"code"}, "client_id": {"browser-fixture"}, "redirect_uri": {redirect},
				"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
				"resource": {issuer + "/mcp"}, "scope": {"mcp"}, "state": {state},
			}
			launchIsolatedEdge(t, edge, extension, issuer+"/oauth/authorize?"+params.Encode())
			deadline := time.Now().Add(20 * time.Second)
			for !bridge.Status().Connected && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
			status := bridge.Status()
			if !status.Connected || len(status.Instances) != 1 {
				t.Fatal("owned OAuth browser did not connect")
			}
			tabs, err := bridge.Tabs(ctx, status.Instances[0].BrowserID)
			if err != nil {
				t.Fatal(err)
			}
			var tabID string
			for _, tab := range tabs {
				if strings.HasPrefix(tab.URL, issuer+"/oauth/authorize") {
					tabID = tab.ID
				}
			}
			if tabID == "" {
				t.Fatal("consent page was not opened in the owned browser")
			}
			observe := func(text string, expectedURL ...string) Snapshot {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for {
					state, err := bridge.Snapshot(ctx, tabID, 40, 3000)
					if err == nil && strings.Contains(state.Text, text) && (len(expectedURL) == 0 || state.URL == expectedURL[0]) {
						return state
					}
					if time.Now().After(deadline) {
						t.Fatalf("browser did not display %q; snapshot=%+v error=%v", text, state, err)
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
			act := func(state Snapshot, name, kind, text string) {
				t.Helper()
				for _, element := range state.Elements {
					if strings.TrimSpace(element.Name) == name {
						if _, err := bridge.Act(ctx, Action{TabID: tabID, Kind: kind, Ref: element.Ref, Text: text}); err != nil {
							t.Fatal(err)
						}
						return
					}
				}
				t.Fatalf("consent element %q missing: %+v", name, state.Elements)
			}
			if scenario.wrongOwner {
				act(observe("Authorize Local Runtime MCP"), "Owner approval key", "set_value", "wrong-fixture-key")
				act(observe("Authorize Local Runtime MCP"), "Approve access", "click", "")
				observe("The owner approval key is incorrect.")
				if callbackCount.Load() != 0 {
					t.Fatal("incorrect owner key reached the callback")
				}
			}
			if scenario.deny {
				act(observe("Authorize Local Runtime MCP"), "Cancel", "click", "")
			} else {
				act(observe("Authorize Local Runtime MCP"), "Owner approval key", "set_value", owner)
				act(observe("Authorize Local Runtime MCP"), "Approve access", "click", "")
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				page, err := bridge.Snapshot(ctx, tabID, 20, 2000)
				t.Fatalf("browser did not reach callback: Origin=%q page=%q text=%q error=%v", approvalOrigin.Load(), page.URL, page.Text, err)
			}
			last := observe("Callback chain complete", final.URL+"/ready")
			if last.URL != final.URL+"/ready" || callbackCount.Load() != 1 {
				t.Fatalf("browser did not finish callback chain exactly once: url=%q want=%q callback_count=%d", last.URL, final.URL+"/ready", callbackCount.Load())
			}
		})
	}
}
