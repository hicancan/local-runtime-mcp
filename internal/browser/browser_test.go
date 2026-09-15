package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestBridgeRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := Start(ctx, config.Browser{Listen: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close(context.Background()) })
	clientErrors := make(chan error, 1)
	go fakeExtension(ctx, bridge.Status().Address, clientErrors)
	deadline := time.Now().Add(3 * time.Second)
	for !bridge.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !bridge.Status().Connected {
		t.Fatal("fake extension did not connect")
	}

	tabs, err := bridge.Tabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 1 || tabs[0].ID != 7 || tabs[0].Title != "Example" {
		t.Fatalf("unexpected tabs: %+v", tabs)
	}
	data, info, err := bridge.Screenshot(ctx, ScreenshotOptions{TabID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "png" || info.MIMEType != "image/png" || info.TabID != 7 {
		t.Fatalf("unexpected screenshot: %q %+v", data, info)
	}
	cancel()
	select {
	case err := <-clientErrors:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
	}
}

func fakeExtension(ctx context.Context, address string, errorsChannel chan<- error) {
	for {
		body := bytes.NewBufferString(`{"instance_id":"test-instance","browser":"test","extension_version":"5.0.0"}`)
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/v1/poll", body)
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			errorsChannel <- err
			return
		}
		if response.StatusCode == http.StatusNoContent {
			response.Body.Close()
			continue
		}
		var command command
		err = json.NewDecoder(response.Body).Decode(&command)
		response.Body.Close()
		if err != nil {
			errorsChannel <- err
			return
		}
		var result any
		switch command.Method {
		case "tabs.list":
			result = []Tab{{ID: 7, Title: "Example", URL: "https://example.com"}}
		case "page.screenshot":
			result = map[string]any{"data_base64": base64.StdEncoding.EncodeToString([]byte("png")), "info": ScreenshotInfo{TabID: 7, MIMEType: "image/png"}}
		case "page.navigate":
			result = Tab{ID: 7, Title: "Navigated", URL: "https://example.com/next"}
		default:
			result = map[string]bool{"success": true}
		}
		payload, _ := json.Marshal(map[string]any{"id": command.ID, "instance_id": "test-instance", "result": result})
		resultRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/v1/result", bytes.NewReader(payload))
		resultRequest.Header.Set("Authorization", "Bearer "+testToken)
		resultRequest.Header.Set("Content-Type", "application/json")
		resultResponse, err := http.DefaultClient.Do(resultRequest)
		if err != nil {
			errorsChannel <- err
			return
		}
		_, _ = io.Copy(io.Discard, resultResponse.Body)
		resultResponse.Body.Close()
	}
}

func TestBridgeRejectsMissingToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := Start(ctx, config.Browser{Listen: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close(context.Background()) })
	response, err := http.Post("http://"+bridge.Status().Address+"/v1/poll", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestInstallExtension(t *testing.T) {
	directory, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := os.ReadFile(filepath.Join(directory, "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(manifest, []byte(`"debugger"`)) || !bytes.Contains(worker, []byte("Page.captureScreenshot")) {
		t.Fatal("installed extension is missing browser-control capabilities")
	}
	for _, forbidden := range [][]byte{[]byte(`"activeTab"`), []byte(`"downloads"`), []byte(`"scripting"`), []byte(`"<all_urls>"`)} {
		if bytes.Contains(manifest, forbidden) {
			t.Fatalf("installed extension contains unnecessary permission %s", forbidden)
		}
	}
}

func TestExtensionJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	directory, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, "--check", filepath.Join(directory, "service_worker.js")).CombinedOutput(); err != nil {
		t.Fatalf("extension service worker is not valid JavaScript: %v\n%s", err, output)
	}
}

func TestBridgeRejectsSecondExtensionInstance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := Start(ctx, config.Browser{Listen: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close(context.Background()) })
	poll := func(requestContext context.Context, instance string) (*http.Response, error) {
		body := bytes.NewBufferString(`{"instance_id":"` + instance + `","extension_version":"` + ExtensionVersion + `"}`)
		request, _ := http.NewRequestWithContext(requestContext, http.MethodPost, "http://"+bridge.Status().Address+"/v1/poll", body)
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(request)
	}
	firstDone := make(chan error, 1)
	go func() {
		response, err := poll(ctx, "first")
		if response != nil {
			response.Body.Close()
		}
		firstDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !bridge.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	second, err := poll(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second instance status = %d, want %d", second.StatusCode, http.StatusConflict)
	}
	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first poll did not stop after cancellation")
	}
}

func TestBridgeRejectsOldExtension(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := Start(ctx, config.Browser{Listen: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close(context.Background()) })
	body := bytes.NewBufferString(`{"instance_id":"old","extension_version":"3.0.0"}`)
	request, _ := http.NewRequest(http.MethodPost, "http://"+bridge.Status().Address+"/v1/poll", body)
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUpgradeRequired || bridge.Status().Connected {
		t.Fatalf("old extension status=%d bridge=%+v", response.StatusCode, bridge.Status())
	}
}

func TestActionValidation(t *testing.T) {
	checked := true
	accepted := false
	zero, two, three := 0.0, 2.0, 3.0
	valid := []Action{{Kind: "click", TabID: 1, Ref: "q1:e1"}, {Kind: "double_click", TabID: 1, ScreenshotID: "p1", X: &zero, Y: &zero}, {Kind: "hover", TabID: 1, Selector: "button"}, {Kind: "drag", TabID: 1, Ref: "q1:e1", ToX: &two, ToY: &three}, {Kind: "type_text", TabID: 1, Selector: "input", Text: "x"}, {Kind: "set_value", TabID: 1, Ref: "q1:e1", Text: "x"}, {Kind: "press_key", TabID: 1, Key: "Control+L"}, {Kind: "scroll", TabID: 1, ScrollY: 500}, {Kind: "select", TabID: 1, Selector: "select", Option: "one"}, {Kind: "check", TabID: 1, Ref: "q1:e1", Checked: &checked}, {Kind: "upload_files", TabID: 1, Selector: "input", Files: []string{"C:/x.txt"}}, {Kind: "handle_dialog", TabID: 1, Accept: &accepted}, {Kind: "evaluate", TabID: 1, Script: "document.title"}}
	for _, action := range valid {
		if err := validateAction(action); err != nil {
			t.Errorf("%+v: %v", action, err)
		}
	}
	if err := validateAction(Action{Kind: "click", TabID: 1}); err == nil {
		t.Fatal("targetless click should fail")
	}
	if err := validateAction(Action{Kind: "check", TabID: 1, Ref: "q1:e1"}); err == nil {
		t.Fatal("check without checked should fail")
	}
	if err := validateAction(Action{Kind: "click", TabID: 1, Ref: "q1:e1", Selector: "button"}); err == nil {
		t.Fatal("ambiguous target should fail")
	}
}

func TestNavigationValidation(t *testing.T) {
	bridge := &Bridge{}
	invalid := []Navigation{{TabID: 0, Kind: "reload"}, {TabID: 1, Kind: "url"}, {TabID: 1, Kind: "reload", URL: "https://example.com"}, {TabID: 1, Kind: "unknown"}}
	for _, navigation := range invalid {
		if _, err := bridge.Navigate(context.Background(), navigation); err == nil {
			t.Errorf("expected navigation %+v to fail", navigation)
		}
	}
}

func TestScreenshotValidation(t *testing.T) {
	bridge := &Bridge{}
	if _, _, err := bridge.Screenshot(context.Background(), ScreenshotOptions{TabID: 1, FullPage: true, ClipW: 10, ClipH: 10}); err == nil {
		t.Fatal("full-page clip should fail")
	}
	if _, _, err := bridge.Screenshot(context.Background(), ScreenshotOptions{TabID: 1, ClipW: 10}); err == nil {
		t.Fatal("incomplete clip should fail")
	}
}
