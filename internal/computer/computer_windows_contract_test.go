//go:build windows && amd64

package computer

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This test owns its GUI fixture. It never types into unrelated applications.
func TestWindowsComputerContractSmoke(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_DESKTOP_SMOKE") != "1" {
		t.Skip("interactive desktop opt-in required")
	}
	title := fmt.Sprintf("LRMCP contract %d", time.Now().UnixNano())
	script := `$ErrorActionPreference='Stop'; Add-Type -AssemblyName System.Windows.Forms; $form=[System.Windows.Forms.Form]::new(); $form.Text='` + title + `'; $form.Width=520; $form.Height=380; $form.BackColor=[System.Drawing.Color]::White; $textbox=[System.Windows.Forms.TextBox]::new(); $textbox.AccessibleName='Contract value'; $textbox.Location=[System.Drawing.Point]::new(30,50); $textbox.Width=260; $form.Controls.Add($textbox); $button=[System.Windows.Forms.Button]::new(); $button.Text='Fixture button'; $button.Location=[System.Drawing.Point]::new(30,100); $form.Controls.Add($button); $form.ShowDialog() | Out-Null`
	fixture := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script)
	fixture.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	var fixtureErrors bytes.Buffer
	fixture.Stderr = &fixtureErrors
	if err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.Process.Kill(); _ = fixture.Wait() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := New(ctx)
	defer controller.Close()
	var target Window
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		targets, err := controller.Targets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, window := range targets.Windows {
			if window.Title == title {
				target = window
				break
			}
		}
		if target.TargetID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if target.TargetID == "" {
		_ = fixture.Process.Kill()
		_ = fixture.Wait()
		t.Fatalf("owned WinForms fixture not discovered: %s", fixtureErrors.String())
	}
	parent := targetHWND(t, target.TargetID)
	user32 := windows.NewLazySystemDLL("user32.dll")
	control, err := controller.Control(ctx, ControlOptions{Kind: "acquire", Label: "Owned GUI contract test"})
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Control(context.Background(), ControlOptions{Kind: "release", ControlID: control.ControlID})
	if _, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "activate", TargetID: target.TargetID}); err != nil {
		t.Fatal(err)
	}
	_, _, _ = user32.NewProc("ShowWindow").Call(parent, 3)
	if _, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "activate", TargetID: target.TargetID}); err != nil {
		t.Fatal(err)
	}
	maximized, _, _ := user32.NewProc("IsZoomed").Call(parent)
	if maximized == 0 {
		t.Fatal("activate restored a maximized fixture")
	}
	_, _, _ = user32.NewProc("ShowWindow").Call(parent, 9)
	_, readOnly, err := controller.State(ctx, StateOptions{TargetID: target.TargetID})
	if err != nil || readOnly.Actionable || readOnly.StateID != "" {
		t.Fatalf("read-only contract: %+v %v", readOnly, err)
	}
	_, state, err := controller.State(ctx, StateOptions{ControlID: control.ControlID, TargetID: target.TargetID})
	if err != nil {
		t.Fatal(err)
	}
	var textbox, button string
	for _, element := range state.Elements {
		if element.Role == "textbox" {
			textbox = element.RefID
		}
		if element.Role == "button" && element.Name == "Fixture button" {
			button = element.RefID
		}
	}
	if textbox == "" || button == "" {
		t.Fatalf("fixture UIA references missing: %+v", state)
	}
	if _, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "set_value", TargetID: target.TargetID, StateID: state.StateID, ElementRef: textbox, Text: "semantic-v10"}); err != nil {
		t.Fatalf("real ValuePattern: %v", err)
	}
	var value string
	callback := syscall.NewCallback(func(hwnd, parameter uintptr) uintptr {
		var class [256]uint16
		_, _, _ = user32.NewProc("GetClassNameW").Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
		if strings.Contains(windows.UTF16ToString(class[:]), "EDIT") {
			var text [128]uint16
			_, _, _ = user32.NewProc("SendMessageW").Call(hwnd, 0x000D, uintptr(len(text)), uintptr(unsafe.Pointer(&text[0])))
			value = windows.UTF16ToString(text[:])
		}
		return 1
	})
	_, _, _ = user32.NewProc("EnumChildWindows").Call(parent, callback, 0)
	if value != "semantic-v10" {
		t.Fatalf("ValuePattern did not update actual fixture edit: %q", value)
	}
	if _, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "press_key", TargetID: target.TargetID, StateID: state.StateID, Key: "A"}); err == nil {
		t.Fatal("old observation accepted after semantic action")
	}
	_, state, err = controller.State(ctx, StateOptions{ControlID: control.ControlID, TargetID: target.TargetID})
	if err != nil {
		t.Fatal(err)
	}
	for _, element := range state.Elements {
		if element.Name == "Fixture button" {
			button = element.RefID
		}
	}
	if _, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "set_value", TargetID: target.TargetID, StateID: state.StateID, ElementRef: button, Text: "unsupported"}); err == nil {
		t.Fatal("button silently used physical fallback for set_value")
	}
	_, state, err = controller.State(ctx, StateOptions{ControlID: control.ControlID, TargetID: target.TargetID})
	if err != nil {
		t.Fatal(err)
	}
	x, y, toX, toY := 40, 200, 180, 200
	dragResult := make(chan error, 1)
	go func() {
		_, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "drag", TargetID: target.TargetID, StateID: state.StateID, X: &x, Y: &y, ToX: &toX, ToY: &toY})
		dragResult <- err
	}()
	native := controller.(*coordinator).native.(*workerController)
	pressed := false
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		native.mu.Lock()
		for _, input := range native.inputs {
			if input.Button == "left" {
				pressed = true
			}
		}
		native.mu.Unlock()
		if pressed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !pressed {
		t.Fatal("drag did not expose tracked input-down state")
	}
	if _, err := controller.Control(ctx, ControlOptions{Kind: "release", ControlID: control.ControlID}); err != nil {
		t.Fatal(err)
	}
	if err := <-dragResult; err == nil {
		t.Fatal("release did not interrupt running drag")
	}
	native.mu.Lock()
	held := len(native.inputs)
	native.mu.Unlock()
	if held != 0 {
		t.Fatalf("injected inputs remained after release: %d", held)
	}
	key, _, _ := user32.NewProc("GetAsyncKeyState").Call(1)
	if key&0x8000 != 0 {
		t.Fatal("left mouse remained pressed after cancelled drag")
	}
	status, _ := controller.Control(ctx, ControlOptions{Kind: "status"})
	if status.Status != "free" {
		t.Fatalf("desktop did not become free: %+v", status)
	}
	// Verify the local indicator belongs to this worker, does not activate, is
	// excluded from feedback, and its Stop event revokes the Go control token.
	foregroundBefore, _, _ := user32.NewProc("GetForegroundWindow").Call()
	control, err = controller.Control(ctx, ControlOptions{Kind: "acquire", Label: "Local Stop contract"})
	if err != nil {
		t.Fatal(err)
	}
	native.mu.Lock()
	workerPID := uint32(native.cmd.Process.Pid)
	native.mu.Unlock()
	var stopWindow uintptr
	findOverlay := syscall.NewCallback(func(hwnd, parameter uintptr) uintptr {
		var pid uint32
		_, _, _ = user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid == workerPID {
			var class [128]uint16
			_, _, _ = user32.NewProc("GetClassNameW").Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
			if windows.UTF16ToString(class[:]) == "LocalRuntimeMCPControl" {
				index, _, _ := user32.NewProc("GetWindowLongPtrW").Call(hwnd, ^uintptr(20))
				if index == 4 {
					stopWindow = hwnd
				}
			}
		}
		return 1
	})
	_, _, _ = user32.NewProc("EnumWindows").Call(findOverlay, 0)
	if stopWindow == 0 {
		t.Fatal("local Stop overlay missing")
	}
	style, _, _ := user32.NewProc("GetWindowLongPtrW").Call(stopWindow, ^uintptr(19))
	if style&0x08000000 == 0 {
		t.Fatal("overlay lacks WS_EX_NOACTIVATE")
	}
	var affinity uint32
	ok, _, _ := user32.NewProc("GetWindowDisplayAffinity").Call(stopWindow, uintptr(unsafe.Pointer(&affinity)))
	if ok == 0 || affinity != 0x11 {
		t.Fatalf("overlay capture exclusion: %x", affinity)
	}
	foregroundAfter, _, _ := user32.NewProc("GetForegroundWindow").Call()
	if foregroundBefore != foregroundAfter {
		t.Fatal("acquire indicator stole foreground")
	}
	capture, screen, err := controller.State(ctx, StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(capture))
	if err != nil {
		t.Fatal(err)
	}
	// The visible blue outline crosses the top desktop edge. WGC must see the
	// underlying pixels rather than the indicator's exact RGB color.
	r, g, b, _ := decoded.At(10, 1).RGBA()
	if r>>8 == 36 && g>>8 == 156 && b>>8 == 232 {
		t.Fatalf("blue indicator leaked into %dx%d feedback", screen.Width, screen.Height)
	}
	_, _, _ = user32.NewProc("PostMessageW").Call(stopWindow, 0x0201, 1, 0)
	until = time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		status, _ = controller.Control(ctx, ControlOptions{Kind: "status"})
		if status.Status == "free" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "free" {
		t.Fatalf("local Stop did not revoke control: %+v", status)
	}
	if _, _, err := controller.State(ctx, StateOptions{ControlID: control.ControlID}); err == nil {
		t.Fatal("local Stop token remained valid")
	}
	control, err = controller.Control(ctx, ControlOptions{Kind: "acquire", Label: "Worker crash cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "activate", TargetID: target.TargetID}); err != nil {
		t.Fatal(err)
	}
	_, state, err = controller.State(ctx, StateOptions{ControlID: control.ControlID, TargetID: target.TargetID})
	if err != nil {
		t.Fatal(err)
	}
	dragResult = make(chan error, 1)
	go func() {
		_, err := controller.Act(ctx, Action{ControlID: control.ControlID, Kind: "drag", TargetID: target.TargetID, StateID: state.StateID, X: &x, Y: &y, ToX: &toX, ToY: &toY})
		dragResult <- err
	}()
	pressed = false
	until = time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		key, _, _ = user32.NewProc("GetAsyncKeyState").Call(1)
		if key&0x8000 != 0 {
			pressed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !pressed {
		t.Fatal("crash fixture drag did not press mouse")
	}
	native.mu.Lock()
	crashed := native.cmd
	native.mu.Unlock()
	if err := crashed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-dragResult; err == nil {
		t.Fatal("worker crash reported action success")
	}
	until = time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		status, _ = controller.Control(ctx, ControlOptions{Kind: "status"})
		if status.Status == "free" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	key, _, _ = user32.NewProc("GetAsyncKeyState").Call(1)
	if status.Status != "free" || key&0x8000 != 0 {
		t.Fatalf("crash supervisor cleanup failed: %+v mouse=%x", status, key)
	}
	control, err = controller.Control(ctx, ControlOptions{Kind: "acquire", Label: "Recovery after worker crash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.State(ctx, StateOptions{ControlID: control.ControlID, TargetID: target.TargetID}); err != nil {
		t.Fatalf("worker did not recover: %v", err)
	}
	if _, err := controller.Control(ctx, ControlOptions{Kind: "release", ControlID: control.ControlID}); err != nil {
		t.Fatal(err)
	}
	t.Log("ValuePattern, unsupported pattern, stale observation, cooperative drag stop, and input cleanup verified on owned fixture")
}

func targetHWND(t *testing.T, id string) uintptr {
	t.Helper()
	parts := strings.Split(id, "-")
	var raw uintptr
	if len(parts) != 4 {
		t.Fatalf("invalid target: %s", id)
	}
	if _, err := fmt.Sscanf(parts[1], "%x", &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
