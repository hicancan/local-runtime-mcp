package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type testWebCodedError struct{}

func (testWebCodedError) Error() string     { return "safe public message" }
func (testWebCodedError) ErrorCode() string { return "provider_unavailable" }

func webFixture() webEnvelope {
	return webEnvelope{InstanceID: "web-profile", BootID: "web-boot", Source: WebSource{
		TabID: "web-boot.sender", Origin: "https://owner.example", URL: "https://owner.example/app", FrameID: 0, DocumentID: "document-one",
	}, Request: WebRequest{ID: "request-one", Method: "models.list", Params: json.RawMessage(`{}`)}}
}

func TestWebBridgeBindsLiveBrowserAndExactDocumentOrigin(t *testing.T) {
	b, ctx := testBridge(t)
	register(t, b, peer("web-profile", "web-boot"))
	calls := 0
	b.SetWebHandler(func(ctx context.Context, source WebSource, request WebRequest, emit func(WebEvent) error) (any, error) {
		calls++
		if source.BrowserID != "web-profile" || source.Origin != "https://owner.example" || source.TabID != "web-boot.sender" {
			t.Errorf("source was not bound to extension identity: %+v", source)
		}
		if err := emit(WebEvent{Type: "text_delta", Data: "hello"}); err != nil {
			return nil, err
		}
		return map[string]any{"models": []string{"allowed"}}, nil
	})
	for _, invalid := range []struct {
		name   string
		mutate func(*webEnvelope)
		status int
	}{
		{"forged origin", func(v *webEnvelope) { v.Source.Origin = "https://another.example" }, 400},
		{"opaque iframe", func(v *webEnvelope) { v.Source.Origin = "null"; v.Source.FrameID = 1 }, 400},
		{"stale boot", func(v *webEnvelope) { v.BootID = "old"; v.Source.TabID = "old.sender" }, 409},
		{"foreign tab", func(v *webEnvelope) { v.Source.TabID = "another.sender" }, 400},
		{"caller browser identity", func(v *webEnvelope) { v.Source.BrowserID = "victim" }, 400},
		{"arbitrary eval", func(v *webEnvelope) { v.Request.Method = "evaluate" }, 400},
		{"array params", func(v *webEnvelope) { v.Request.Params = json.RawMessage(`[]`) }, 400},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			v := webFixture()
			invalid.mutate(&v)
			r, err := postBridge(ctx, b, "web", v)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			if r.StatusCode != invalid.status {
				t.Fatalf("HTTP %d", r.StatusCode)
			}
		})
	}
	if calls != 0 {
		t.Fatal("invalid sender invoked owner handler")
	}
	r, err := postBridge(ctx, b, "web", webFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-ndjson") || !strings.Contains(string(body), `"type":"text_delta"`) || !strings.Contains(string(body), `"type":"result"`) {
		t.Fatalf("unexpected streamed response: %d %s", r.StatusCode, body)
	}
	if calls != 1 {
		t.Fatalf("handler called %d times", calls)
	}
}

func TestWebBridgeDefaultDenyAndOwnerRejection(t *testing.T) {
	b, ctx := testBridge(t)
	register(t, b, peer("web-profile", "web-boot"))
	r, err := postBridge(ctx, b, "web", webFixture())
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured web access HTTP %d", r.StatusCode)
	}
	b.SetWebHandler(func(context.Context, WebSource, WebRequest, func(WebEvent) error) (any, error) {
		return nil, errors.New("origin has no owner grant")
	})
	r, err = postBridge(ctx, b, "web", webFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), `"type":"error"`) || strings.Contains(string(body), `"type":"result"`) {
		t.Fatalf("owner rejection not preserved: %s", body)
	}
	unauthorized, err := http.Post("http://"+b.Status().Address+"/v1/web", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatal("page request without extension token admitted")
	}
}

func TestWebBridgeDisconnectCancelsModelContext(t *testing.T) {
	b, ctx := testBridge(t)
	register(t, b, peer("web-profile", "web-boot"))
	canceled := make(chan struct{})
	b.SetWebHandler(func(ctx context.Context, _ WebSource, _ WebRequest, emit func(WebEvent) error) (any, error) {
		if err := emit(WebEvent{Type: "keepalive"}); err != nil {
			return nil, err
		}
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	})
	requestCtx, cancel := context.WithCancel(ctx)
	r, err := postBridge(requestCtx, b, "web", webFixture())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if _, err = bufio.NewReader(r.Body).ReadString('\n'); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	r.Body.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("web disconnect did not cancel host model request")
	}
}

func TestWebBridgePreservesPublicErrorCode(t *testing.T) {
	b, ctx := testBridge(t)
	register(t, b, peer("web-profile", "web-boot"))
	b.SetWebHandler(func(context.Context, WebSource, WebRequest, func(WebEvent) error) (any, error) {
		return nil, testWebCodedError{}
	})
	r, err := postBridge(ctx, b, "web", webFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var result struct{ Type, Error, Code string }
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Type != "error" || result.Code != "provider_unavailable" || result.Error != "safe public message" {
		t.Fatalf("public code lost: %+v", result)
	}
}
