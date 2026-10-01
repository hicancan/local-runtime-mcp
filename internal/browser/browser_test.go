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
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

const testToken = "0123456789abcdef0123456789abcdef"

type testConnectionKey struct {
	bridge   *Bridge
	id, boot string
}

var testConnections sync.Map

const testOrigin = "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
	r, err := websocket.Dial("ws://"+b.Status().Address+"/v1/connect", "", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := websocket.JSON.Send(r, connectionMessage{Type: "authenticate", Token: testToken, Peer: p}); err != nil {
		t.Fatal(err)
	}
	var ready connectionMessage
	if err := websocket.JSON.Receive(r, &ready); err != nil || ready.Type != "ready" {
		t.Fatalf("authentication: %+v %v", ready, err)
	}
	testConnections.Store(testConnectionKey{b, p.InstanceID, p.BootID}, r)
	t.Cleanup(func() { testConnections.Delete(testConnectionKey{b, p.InstanceID, p.BootID}) })
}
func pollCommand(t *testing.T, b *Bridge, p Peer) command {
	t.Helper()
	value, ok := testConnections.Load(testConnectionKey{b, p.InstanceID, p.BootID})
	if !ok {
		t.Fatal("unregistered test connection")
	}
	r := value.(*websocket.Conn)
	_ = r.SetReadDeadline(time.Now().Add(time.Second))
	var c command
	if err := websocket.JSON.Receive(r, &c); err != nil {
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
func TestBridgeExpiredQueueEntriesNeverDispatch(t *testing.T) {
	b, _ := testBridge(t)
	a := peer("profile", "boot")
	register(t, b, a)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	b.mu.Lock()
	b.pending["expired"] = &pendingCall{browserID: a.InstanceID, bootID: a.BootID, ctx: canceled, cancel: cancel}
	current := b.instances[a.InstanceID]
	b.mu.Unlock()
	current.commands <- command{ID: "expired", BootID: a.BootID, Deadline: time.Now().Add(-time.Second).UnixMilli(), Method: "tabs.list"}
	socketValue, _ := testConnections.Load(testConnectionKey{b, a.InstanceID, a.BootID})
	socket := socketValue.(*websocket.Conn)
	_ = socket.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	var value command
	if websocket.JSON.Receive(socket, &value) == nil {
		t.Fatalf("expired command dispatched: %+v", value)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) != 0 {
		t.Fatal("expired queue entry was not removed")
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
	for _, test := range []struct {
		name, token string
		p           Peer
		want        string
	}{
		{"missing", "", peer("missing", "boot"), "unauthorized"},
		{"wrong", "incorrect credential", peer("wrong", "boot"), "unauthorized"},
		{"old", testToken, Peer{InstanceID: "old", BootID: "boot", ExtensionVersion: "3.0.0"}, "version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket, err := websocket.Dial("ws://"+b.Status().Address+"/v1/connect", "", testOrigin)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			if err := websocket.JSON.Send(socket, connectionMessage{Type: "authenticate", Token: test.token, Peer: test.p}); err != nil {
				t.Fatal(err)
			}
			var response connectionMessage
			if err := websocket.JSON.Receive(socket, &response); err != nil || response.Type != "error" || !strings.Contains(response.Error, test.want) {
				t.Fatalf("response=%+v error=%v", response, err)
			}
		})
	}
	for _, origin := range []string{"https://example.com", "http://" + b.Status().Address, "chrome-extension://invalid"} {
		if socket, err := websocket.Dial("ws://"+b.Status().Address+"/v1/connect", "", origin); err == nil {
			socket.Close()
			t.Fatalf("accepted origin %s", origin)
		}
	}
	for _, path := range []string{"hello", "poll", "heartbeat"} {
		r, err := postBridge(context.Background(), b, path, peer("legacy", "boot"))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Fatalf("legacy route %s was retained: %d", path, r.StatusCode)
		}
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
	for _, name := range []string{"service_worker.js", "connection.js", "scheduler.js", "options.js"} {
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
