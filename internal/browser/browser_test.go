package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testBridge(t *testing.T) (*Bridge, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b, err := Start(ctx, config.Browser{Listen: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b, ctx
}
func postBridge(ctx context.Context, b *Bridge, path string, payload any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	r, _ := http.NewRequestWithContext(ctx, "POST", "http://"+b.Status().Address+"/v1/"+path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	return http.DefaultClient.Do(r)
}
func peer(id, boot string) Peer {
	return Peer{InstanceID: id, BootID: boot, Label: id, ExtensionVersion: ExtensionVersion, Browser: "test"}
}
func register(t *testing.T, b *Bridge, p Peer) {
	t.Helper()
	r, err := postBridge(context.Background(), b, "hello", p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 204 {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("hello %d: %s", r.StatusCode, body)
	}
}
func pollCommand(t *testing.T, b *Bridge, p Peer) command {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := postBridge(ctx, b, "poll", p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var c command
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c
}
func finishCommand(t *testing.T, b *Bridge, p Peer, c command, value any) int {
	t.Helper()
	data, _ := json.Marshal(value)
	r, err := postBridge(context.Background(), b, "result", response{ID: c.ID, InstanceID: p.InstanceID, BootID: p.BootID, Result: data})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	return r.StatusCode
}

func TestBridgeMultipleInstancesRoutesReversedResults(t *testing.T) {
	b, ctx := testBridge(t)
	a, c := peer("a", "boot-a"), peer("c", "boot-c")
	register(t, b, a)
	register(t, b, c)
	if len(b.Status().Instances) != 2 || !b.Status().Connected {
		t.Fatal(b.Status())
	}
	type answer struct {
		tabs []Tab
		err  error
	}
	ar, cr := make(chan answer, 1), make(chan answer, 1)
	go func() { tabs, err := b.Tabs(ctx, a.InstanceID); ar <- answer{tabs, err} }()
	go func() { tabs, err := b.Tabs(ctx, c.InstanceID); cr <- answer{tabs, err} }()
	ac, cc := pollCommand(t, b, a), pollCommand(t, b, c)
	if status := finishCommand(t, b, c, ac, []Tab{{ID: "boot-a.tab"}}); status != 409 {
		t.Fatal(status)
	}
	if status := finishCommand(t, b, c, cc, []Tab{{ID: "boot-c.tab", Title: "C"}}); status != 204 {
		t.Fatal(status)
	}
	if status := finishCommand(t, b, a, ac, []Tab{{ID: "boot-a.tab", Title: "A"}}); status != 204 {
		t.Fatal(status)
	}
	aa, ca := <-ar, <-cr
	if aa.err != nil || ca.err != nil || aa.tabs[0].Title != "A" || ca.tabs[0].Title != "C" {
		t.Fatalf("%+v %+v", aa, ca)
	}
	done := make(chan error, 1)
	go func() {
		data, info, err := b.Screenshot(ctx, ScreenshotOptions{TabID: aa.tabs[0].ID})
		if err == nil && (string(data) != "png" || info.TabID != aa.tabs[0].ID) {
			err = io.ErrUnexpectedEOF
		}
		done <- err
	}()
	item := pollCommand(t, b, a)
	finishCommand(t, b, a, item, map[string]any{"data_base64": base64.StdEncoding.EncodeToString([]byte("png")), "info": ScreenshotInfo{TabID: aa.tabs[0].ID, MIMEType: "image/png"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestBridgeRestartInvalidatesHandlesAndPending(t *testing.T) {
	b, ctx := testBridge(t)
	a := peer("profile", "old")
	register(t, b, a)
	done := make(chan error, 1)
	go func() { _, err := b.Tabs(ctx, a.InstanceID); done <- err }()
	item := pollCommand(t, b, a)
	newer := peer("profile", "new")
	register(t, b, newer)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "restarted") {
		t.Fatal(err)
	}
	if status := finishCommand(t, b, a, item, []Tab{{ID: "old.tab"}}); status != 409 {
		t.Fatal(status)
	}
	if b.registerTabs(newer.InstanceID, []Tab{{ID: "old.tab"}}) == nil {
		t.Fatal("accepted old-generation handle")
	}
	if err := b.registerTabs(newer.InstanceID, []Tab{{ID: "new.tab"}}); err != nil {
		t.Fatal(err)
	}
	register(t, b, peer("profile", "third"))
	if _, err := b.route("new.tab"); err == nil {
		t.Fatal("old handle routable")
	}
}
func TestBridgeQueuedCancellationNeverDispatches(t *testing.T) {
	b, _ := testBridge(t)
	a := peer("profile", "boot")
	register(t, b, a)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.Tabs(ctx, a.InstanceID); err == nil {
		t.Fatal("not canceled")
	}
	b.mu.Lock()
	pending := len(b.pending)
	b.mu.Unlock()
	if pending != 0 {
		t.Fatal("canceled request pending")
	}
	ctx2, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if r, err := postBridge(ctx2, b, "poll", a); err == nil {
		r.Body.Close()
		t.Fatal("canceled command dispatched")
	}
}
func TestDesktopReservationHeldUntilExecutionAcknowledgement(t *testing.T) {
	b, _ := testBridge(t)
	a := peer("profile", "boot")
	register(t, b, a)
	if err := b.registerTabs(a.InstanceID, []Tab{{ID: "boot.tab"}}); err != nil {
		t.Fatal(err)
	}
	releases := make(chan struct{}, 1)
	b.SetDesktopGate(func(ctx context.Context, _ string) (context.Context, func(), error) {
		return ctx, func() { releases <- struct{}{} }, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.Act(ctx, Action{TabID: "boot.tab", Kind: "evaluate", Script: "1"}); done <- err }()
	item := pollCommand(t, b, a)
	r, err := postBridge(context.Background(), b, "authorize", response{ID: item.ID, InstanceID: a.InstanceID, BootID: a.BootID})
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("not canceled")
	}
	select {
	case <-releases:
		t.Fatal("released before execution ack")
	default:
	}
	canceled := pollCommand(t, b, a)
	if canceled.Method != "cancel" {
		t.Fatal(canceled)
	}
	finishCommand(t, b, a, item, ActionResult{TabID: "boot.tab", Success: true})
	select {
	case <-releases:
	case <-time.After(time.Second):
		t.Fatal("not released")
	}
}
func TestBridgeRejectsMissingTokenAndOldExtension(t *testing.T) {
	b, _ := testBridge(t)
	r, err := http.Post("http://"+b.Status().Address+"/v1/hello", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodPost, "http://"+b.Status().Address+"/v1/hello", strings.NewReader(`{}`))
	request.Header.Set("Authorization", testToken)
	r, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal("accepted a token without Bearer prefix")
	}
	p := peer("old", "boot")
	p.ExtensionVersion = "3.0.0"
	r, err = postBridge(context.Background(), b, "hello", p)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 426 {
		t.Fatal(r.StatusCode)
	}
}

func TestDesktopLocalStopCancelsBrowserButWaitsForExecutionAck(t *testing.T) {
	b, _ := testBridge(t)
	a := peer("profile", "boot")
	register(t, b, a)
	if err := b.registerTabs(a.InstanceID, []Tab{{ID: "boot.tab"}}); err != nil {
		t.Fatal(err)
	}
	controlled, stopControl := context.WithCancel(context.Background())
	defer stopControl()
	released := make(chan struct{}, 1)
	b.SetDesktopGate(func(context.Context, string) (context.Context, func(), error) {
		return controlled, func() { released <- struct{}{} }, nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := b.Act(context.Background(), Action{TabID: "boot.tab", Kind: "evaluate", Script: "1"})
		done <- err
	}()
	item := pollCommand(t, b, a)
	r, err := postBridge(context.Background(), b, "authorize", response{ID: item.ID, InstanceID: a.InstanceID, BootID: a.BootID})
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	stopControl()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("local Stop did not cancel MCP call")
		}
	case <-time.After(time.Second):
		t.Fatal("local Stop did not reach Browser")
	}
	select {
	case <-released:
		t.Fatal("released before execution cancellation acknowledged")
	default:
	}
	cancelCommand := pollCommand(t, b, a)
	if cancelCommand.Method != "cancel" {
		t.Fatal(cancelCommand)
	}
	finishCommand(t, b, a, item, ActionResult{TabID: "boot.tab", Success: true})
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("reservation not released after ack")
	}
}
func TestInstallExtension(t *testing.T) {
	dir, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := os.ReadFile(filepath.Join(dir, "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(manifest, []byte(`"debugger"`)) || !bytes.Contains(worker, []byte("Page.captureScreenshot")) {
		t.Fatal("missing control")
	}
	for _, f := range []string{`"activeTab"`, `"downloads"`, `"scripting"`, `"<all_urls>"`} {
		if bytes.Contains(manifest, []byte(f)) {
			t.Fatal(f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "scheduler.js")); err != nil {
		t.Fatal(err)
	}
}
func TestExtensionJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"service_worker.js", "scheduler.js", "options.js"} {
		if out, err := exec.Command(node, "--check", filepath.Join(dir, name)).CombinedOutput(); err != nil {
			t.Fatalf("%s %v\n%s", name, err, out)
		}
	}
}
func TestActionValidation(t *testing.T) {
	if err := validateAction(Action{Kind: "click", TabID: "tab", Ref: "q:e"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Action{{Kind: "click", TabID: "tab"}, {Kind: "click", Ref: "q:e"}, {Kind: "check", TabID: "tab", Ref: "q:e"}, {Kind: "invalid", TabID: "tab"}} {
		if validateAction(a) == nil {
			t.Fatalf("accepted %+v", a)
		}
	}
}
