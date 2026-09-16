//go:build windows

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

// TestEdgeExtensionEndToEnd is opt-in because it launches a real isolated Edge
// profile. It exercises the packaged MV3 extension, CDP, page projection,
// observation epochs, native screenshots, actions, and navigation as one path.
func TestEdgeExtensionEndToEnd(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_BROWSER_E2E") != "1" {
		t.Skip("set LOCAL_RUNTIME_MCP_BROWSER_E2E=1 to launch isolated Edge")
	}
	edge := findEdge()
	if edge == "" {
		t.Skip("Microsoft Edge was not found")
	}

	child := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(writer, `<!doctype html><title>child</title><p id="status">child ready</p><button id="child" onclick="document.querySelector('#status').textContent='child clicked'">Child action</button>`)
	}))
	defer child.Close()
	childURL := strings.Replace(child.URL, "127.0.0.1", "child.local-runtime-mcp.invalid", 1)
	page := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(writer, `<!doctype html><title>LRMCP E2E</title><main><h1>ready</h1><button id="button" onclick="document.querySelector('h1').textContent='clicked'">Run</button><button id="dialog" onclick="document.querySelector('h1').textContent=prompt('value','default')||'dismissed'">Dialog</button><iframe src=%q></iframe></main>`, childURL)
	}))
	defer page.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := Start(ctx, config.Browser{Listen: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close(context.Background()) })

	extension, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfigureExtension(extension, bridge.Status().Address, testToken); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "edge-profile")
	logPath := filepath.Join(t.TempDir(), "edge.log")
	command := exec.Command(edge,
		"--headless=new", "--disable-gpu", "--silent-debugger-extension-api", "--no-first-run", "--no-default-browser-check", "--enable-logging", "--v=1", "--log-file="+logPath,
		"--host-resolver-rules=MAP child.local-runtime-mcp.invalid 127.0.0.1",
		"--user-data-dir="+profile,
		"--disable-extensions-except="+extension,
		"--load-extension="+extension,
		page.URL,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})

	deadline := time.Now().Add(20 * time.Second)
	for !bridge.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !bridge.Status().Connected {
		t.Fatal("isolated Edge extension did not connect to the bridge")
	}

	callContext, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	tabs, err := bridge.Tabs(callContext)
	if err != nil {
		t.Fatal(err)
	}
	var tabID int
	for _, tab := range tabs {
		if strings.HasPrefix(tab.URL, page.URL) {
			tabID = tab.ID
			break
		}
	}
	if tabID == 0 {
		t.Fatalf("test page was not controllable: %+v", tabs)
	}

	var snapshot Snapshot
	snapshotDeadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err = bridge.Snapshot(callContext, tabID, 20, 1000)
		if err == nil && strings.Contains(snapshot.Text, "Child action") {
			break
		}
		if time.Now().After(snapshotDeadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		bridge.mu.Lock()
		t.Logf("bridge diagnostics: queued_commands=%d pending=%d last_seen=%s", len(bridge.commands), len(bridge.pending), bridge.lastSeen.Format(time.RFC3339Nano))
		bridge.mu.Unlock()
		if log, readErr := os.ReadFile(logPath); readErr == nil {
			var relevant []string
			for _, line := range strings.Split(string(log), "\n") {
				upper := strings.ToUpper(line)
				if strings.Contains(upper, "CONSOLE") || strings.Contains(upper, "ERROR") || strings.Contains(upper, "EXTENSION") {
					relevant = append(relevant, line)
				}
			}
			t.Logf("Relevant Edge log:\n%s", strings.Join(relevant, "\n"))
		}
		t.Fatal(err)
	}
	if snapshot.PageEpoch == "" || !strings.Contains(snapshot.Text, "ready") || !strings.Contains(snapshot.Text, "Child action") || len(snapshot.Elements) < 2 {
		t.Fatalf("unexpected initial snapshot: %+v", snapshot)
	}
	var childRef string
	for _, element := range snapshot.Elements {
		if element.Name == "Child action" {
			childRef = element.Ref
			break
		}
	}
	if childRef == "" {
		t.Fatalf("cross-origin child button is missing from AX snapshot: %+v", snapshot.Elements)
	}
	if _, err := bridge.Act(callContext, Action{Kind: "click", TabID: tabID, Ref: childRef}); err != nil {
		t.Fatalf("cross-origin child action failed: %v", err)
	}
	afterChild, err := bridge.Snapshot(callContext, tabID, 20, 1000)
	if err != nil || !strings.Contains(afterChild.Text, "child clicked") || afterChild.PageEpoch == snapshot.PageEpoch {
		t.Fatalf("child action snapshot=%+v err=%v", afterChild, err)
	}
	var runRef string
	for _, element := range afterChild.Elements {
		if element.Name == "Run" {
			runRef = element.Ref
			break
		}
	}
	if runRef == "" {
		t.Fatalf("main-frame Run button is missing after child action: %+v", afterChild.Elements)
	}
	image, screenshot, err := bridge.Screenshot(callContext, ScreenshotOptions{TabID: tabID})
	if err != nil {
		t.Fatal(err)
	}
	if len(image) < 8 || string(image[:8]) != "\x89PNG\r\n\x1a\n" || screenshot.ScreenshotID == "" {
		t.Fatalf("unexpected screenshot: %+v bytes=%d", screenshot, len(image))
	}
	if len(image) < 8 || string(image[:8]) != "\x89PNG\r\n\x1a\n" || screenshot.PageEpoch != afterChild.PageEpoch || screenshot.ScreenshotID == "" {
		t.Fatalf("snapshot/screenshot epoch mismatch: snapshot=%+v screenshot=%+v bytes=%d", afterChild, screenshot, len(image))
	}
	if _, err := bridge.Act(callContext, Action{Kind: "click", TabID: tabID, Ref: runRef}); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	if _, err := bridge.Act(callContext, Action{Kind: "click", TabID: tabID, ScreenshotID: screenshot.ScreenshotID, X: &zero, Y: &zero}); err == nil {
		t.Fatal("action accepted an observation invalidated by the preceding action")
	}
	updated, err := bridge.Snapshot(callContext, tabID, 20, 1000)
	if err != nil || !strings.Contains(updated.Text, "clicked") || updated.PageEpoch == snapshot.PageEpoch {
		t.Fatalf("updated snapshot=%+v err=%v", updated, err)
	}
	var dialogRef string
	for _, element := range updated.Elements {
		if element.Name == "Dialog" {
			dialogRef = element.Ref
			break
		}
	}
	if dialogRef == "" {
		t.Fatalf("dialog button is missing after main-frame action: %+v", updated.Elements)
	}
	if _, err := bridge.Act(callContext, Action{Kind: "click", TabID: tabID, Ref: dialogRef}); err != nil {
		t.Fatalf("dialog-opening click did not return: %v", err)
	}
	accept := true
	if _, err := bridge.Act(callContext, Action{Kind: "handle_dialog", TabID: tabID, Accept: &accept, PromptText: "accepted"}); err != nil {
		t.Fatalf("handle dialog failed: %v", err)
	}
	afterDialog, err := bridge.Snapshot(callContext, tabID, 20, 1000)
	if err != nil || !strings.Contains(afterDialog.Text, "accepted") {
		t.Fatalf("dialog result snapshot=%+v err=%v", afterDialog, err)
	}
	if _, err := bridge.Navigate(callContext, Navigation{TabID: tabID, Kind: "reload"}); err != nil {
		t.Fatal(err)
	}
}

func findEdge() string {
	for _, path := range []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
	} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}
