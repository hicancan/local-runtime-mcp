//go:build windows

package browser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"golang.org/x/sys/windows"
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
	launchContainedEdge(t, edge, []string{
		"--headless=new", "--disable-gpu", "--silent-debugger-extension-api", "--no-first-run", "--no-default-browser-check", "--enable-logging=stderr", "--v=1",
		"--host-resolver-rules=MAP child.local-runtime-mcp.invalid 127.0.0.1",
		"--user-data-dir=" + profile, "--disable-extensions-except=" + extension, "--load-extension=" + extension, page.URL,
	}, logPath, filepath.Dir(profile), filepath.Dir(logPath))

	deadline := time.Now().Add(20 * time.Second)
	for !bridge.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !bridge.Status().Connected {
		t.Fatal("isolated Edge extension did not connect to the bridge")
	}

	callContext, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	tabs, err := bridge.Tabs(callContext, bridge.Status().Instances[0].BrowserID)
	if err != nil {
		t.Fatal(err)
	}
	var tabID string
	for _, tab := range tabs {
		if strings.HasPrefix(tab.URL, page.URL) {
			tabID = tab.ID
			break
		}
	}
	if tabID == "" {
		t.Fatalf("test page was not controllable: %+v", tabs)
	}
	firstBrowser := bridge.Status().Instances[0].BrowserID
	launchIsolatedEdge(t, edge, extension, page.URL)
	multipleDeadline := time.Now().Add(20 * time.Second)
	for len(bridge.Status().Instances) < 2 && time.Now().Before(multipleDeadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if len(bridge.Status().Instances) != 2 {
		t.Fatalf("second profile did not connect: %+v", bridge.Status())
	}
	var secondBrowser string
	for _, instance := range bridge.Status().Instances {
		if instance.BrowserID != firstBrowser {
			secondBrowser = instance.BrowserID
		}
	}
	secondTabs, err := bridge.Tabs(callContext, secondBrowser)
	if err != nil {
		t.Fatal(err)
	}
	var secondTab string
	for _, tab := range secondTabs {
		if strings.HasPrefix(tab.URL, page.URL) {
			secondTab = tab.ID
		}
	}
	if secondTab == "" || secondTab == tabID {
		t.Fatalf("profile handles collide: first=%s second=%s", tabID, secondTab)
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
		t.Logf("bridge diagnostics: %+v", bridge.Status())
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
	secondSnapshot, err := bridge.Snapshot(callContext, secondTab, 20, 1000)
	if err != nil || strings.Contains(secondSnapshot.Text, "child clicked") || !strings.Contains(secondSnapshot.Text, "ready") {
		t.Fatalf("profile isolation snapshot=%+v err=%v", secondSnapshot, err)
	}
	background, err := bridge.Open(callContext, firstBrowser, page.URL, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if background.Active {
		t.Fatal("background open activated the tab")
	}

	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		once.Do(func() { close(started) })
		select {
		case <-release:
			fmt.Fprint(w, "released")
		case <-r.Context().Done():
		}
	}))
	defer blocking.Close()
	longDone := make(chan error, 1)
	go func() {
		_, err := bridge.Act(callContext, Action{TabID: background.ID, Kind: "evaluate", Script: fmt.Sprintf("fetch(%q).then(r=>r.text())", blocking.URL)})
		longDone <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("long browser task did not start")
	}
	queuedContext, queuedCancel := context.WithCancel(callContext)
	queuedDone := make(chan error, 1)
	go func() {
		_, err := bridge.Act(queuedContext, Action{TabID: background.ID, Kind: "evaluate", Script: "globalThis.canceledTaskRan=true"})
		queuedDone <- err
	}()
	// Ensure the command crossed the Bridge intake boundary before canceling it.
	queuedDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(queuedDeadline) {
		bridge.mu.Lock()
		dispatched := 0
		for _, pending := range bridge.pending {
			if pending.dispatched {
				dispatched++
			}
		}
		bridge.mu.Unlock()
		if dispatched >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	queuedCancel()
	if err := <-queuedDone; err == nil {
		close(release)
		t.Fatal("queued task did not cancel")
	}
	// A separate tab must complete while the first tab remains blocked.
	parallelDone := make(chan error, 1)
	go func() { _, err := bridge.Snapshot(callContext, tabID, 20, 1000); parallelDone <- err }()
	select {
	case err := <-parallelDone:
		if err != nil {
			close(release)
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		close(release)
		t.Fatal("busy lane starved independent tab")
	}
	close(release)
	if err := <-longDone; err != nil {
		t.Fatal(err)
	}
	value, err := bridge.Act(callContext, Action{TabID: background.ID, Kind: "evaluate", Script: "globalThis.canceledTaskRan===true"})
	if err != nil || value.Value != false {
		t.Fatalf("canceled queued task executed: %+v %v", value, err)
	}
	if err := bridge.CloseTab(callContext, background.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Snapshot(callContext, background.ID, 20, 1000); err == nil {
		t.Fatal("closed tab handle accepted")
	}
	openStarted, finishOpen := make(chan struct{}), make(chan struct{})
	var openOnce sync.Once
	slowPage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openOnce.Do(func() { close(openStarted) })
		select {
		case <-finishOpen:
			fmt.Fprint(w, "<!doctype html><title>late page</title><p>loaded</p>")
		case <-r.Context().Done():
		}
	}))
	defer slowPage.Close()
	openDone := make(chan error, 1)
	go func() { _, err := bridge.Open(callContext, firstBrowser, slowPage.URL, false, ""); openDone <- err }()
	select {
	case <-openStarted:
	case <-time.After(5 * time.Second):
		close(finishOpen)
		t.Fatal("slow open did not begin")
	}
	listed, err := bridge.Tabs(callContext, firstBrowser)
	if err != nil {
		close(finishOpen)
		t.Fatal(err)
	}
	var loadingTab string
	for _, tab := range listed {
		if strings.HasPrefix(tab.URL, slowPage.URL) {
			loadingTab = tab.ID
		}
	}
	if loadingTab == "" {
		close(finishOpen)
		t.Fatalf("opening target missing: %+v", listed)
	}
	loadingDone := make(chan error, 1)
	go func() { _, err := bridge.Snapshot(callContext, loadingTab, 20, 1000); loadingDone <- err }()
	// An unrelated existing tab is a synchronization barrier and must stay runnable.
	if _, err := bridge.Snapshot(callContext, tabID, 20, 1000); err != nil {
		close(finishOpen)
		t.Fatal(err)
	}
	select {
	case err := <-loadingDone:
		close(finishOpen)
		t.Fatalf("observation raced initial open: %v", err)
	default:
	}
	close(finishOpen)
	if err := <-openDone; err != nil {
		t.Fatal(err)
	}
	if err := <-loadingDone; err != nil {
		t.Fatal(err)
	}
}

func launchIsolatedEdge(t *testing.T, edge, extension, url string) {
	t.Helper()
	profile := filepath.Join(t.TempDir(), "edge-profile")
	launchContainedEdge(t, edge, []string{
		"--headless=new", "--disable-gpu", "--silent-debugger-extension-api", "--no-first-run", "--no-default-browser-check", "--enable-logging=stderr",
		"--host-resolver-rules=MAP child.local-runtime-mcp.invalid 127.0.0.1", "--user-data-dir=" + profile,
		"--disable-extensions-except=" + extension, "--load-extension=" + extension, url,
	}, filepath.Join(filepath.Dir(profile), "edge.log"), filepath.Dir(profile))
}

// Suspend before assignment: Chromium can spawn children immediately, before
// exec.Cmd.Start followed by AssignProcessToJobObject would contain them.
func launchContainedEdge(t *testing.T, edge string, args []string, logPath string, directories ...string) {
	t.Helper()
	// Keep the log file handle in Go. Chromium receives an anonymous pipe,
	// so background brokers cannot retain a handle to the test's disk log.
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logReader, logWriter, err := os.Pipe()
	if err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	defer logWriter.Close()
	logDone := make(chan struct{})
	go func() { _, _ = io.Copy(logFile, logReader); close(logDone) }()
	var finishLogOnce sync.Once
	finishLog := func() {
		finishLogOnce.Do(func() {
			_ = logReader.Close()
			<-logDone
			_ = logFile.Close()
		})
	}
	t.Cleanup(finishLog)
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	for _, handle := range []windows.Handle{windows.Handle(logWriter.Fd()), windows.Handle(input.Fd())} {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			t.Fatal(err)
		}
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information))); err != nil {
		_ = windows.CloseHandle(job)
		t.Fatal(err)
	}
	application, err := windows.UTF16PtrFromString(edge)
	if err != nil {
		_ = windows.CloseHandle(job)
		t.Fatal(err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{edge}, args...)))
	if err != nil {
		_ = windows.CloseHandle(job)
		t.Fatal(err)
	}
	startup := windows.StartupInfo{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.StdInput = windows.Handle(input.Fd())
	startup.StdOutput = windows.Handle(logWriter.Fd())
	startup.StdErr = windows.Handle(logWriter.Fd())
	process := windows.ProcessInformation{}
	if err := windows.CreateProcess(application, commandLine, nil, nil, true,
		windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW, nil, nil, &startup, &process); err != nil {
		_ = windows.CloseHandle(job)
		t.Fatal(err)
	}
	defer windows.CloseHandle(process.Thread)
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		_ = windows.TerminateProcess(process.Process, 1)
		_, _ = windows.WaitForSingleObject(process.Process, 5000)
		_ = windows.CloseHandle(process.Process)
		_ = windows.CloseHandle(job)
		t.Fatalf("contain suspended Edge process: %v", err)
	}
	// Cleanup is registered before resuming, including the resume-failure path.
	t.Cleanup(func() {
		defer windows.CloseHandle(process.Process)
		defer windows.CloseHandle(job)
		if err := windows.TerminateJobObject(job, 1); err != nil {
			t.Errorf("terminate isolated Edge tree: %v", err)
		}
		_, _ = windows.WaitForSingleObject(process.Process, 5000)
		deadline := time.Now().Add(10 * time.Second)
		for {
			var accounting struct {
				TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
				TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
			}
			if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
				uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
				t.Errorf("query isolated Edge tree: %v", err)
				break
			}
			if accounting.ActiveProcesses == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("isolated Edge tree still has %d active processes", accounting.ActiveProcesses)
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		finishLog()
		for _, directory := range directories {
			deadline := time.Now().Add(5 * time.Second)
			for {
				err := os.RemoveAll(directory)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Errorf("remove owned Edge fixture directory: %v", err)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	})
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		t.Fatalf("resume contained Edge: %v", err)
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
