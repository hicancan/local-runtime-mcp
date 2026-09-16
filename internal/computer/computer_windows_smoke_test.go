//go:build windows

package computer

import (
	"context"
	"os"
	"testing"
)

// TestWindowsDesktopSmoke is opt-in because CI runners do not have an
// interactive desktop. It validates the real signed-in session before release.
func TestWindowsDesktopSmoke(t *testing.T) {
	if os.Getenv("LRMCP_DESKTOP_SMOKE") != "1" {
		t.Skip("set LRMCP_DESKTOP_SMOKE=1 in an interactive Windows session")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := New(ctx)
	defer controller.Close()
	targets, err := controller.Targets(context.Background())
	if err != nil {
		t.Fatalf("window discovery failed: targets=%+v err=%v", targets, err)
	}
	var active string
	for _, target := range targets.Windows {
		if target.Active {
			active = target.TargetID
			break
		}
	}
	if active != "" {
		windowImage, windowState, err := controller.State(context.Background(), StateOptions{TargetID: active})
		if err != nil || len(windowImage) < 8 || windowState.TargetID != active || windowState.StateID == "" {
			t.Fatalf("foreground WGC/UIA state failed: bytes=%d state=%+v err=%v", len(windowImage), windowState, err)
		}
		if len(windowState.Elements) == 0 {
			t.Fatal("foreground UI Automation projection returned no interactive elements")
		}
	}
	data, info, err := controller.State(context.Background(), StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" || info.Width < 1 || info.Height < 1 {
		t.Fatalf("invalid desktop capture: bytes=%d info=%+v", len(data), info)
	}
	if len(info.Elements) == 0 {
		t.Fatal("desktop observation returned no UI Automation projection for the foreground application")
	}
	result, err := controller.Act(context.Background(), Action{Kind: "move", StateID: info.StateID, X: &info.CursorX, Y: &info.CursorY})
	if err != nil || !result.Success {
		t.Fatalf("desktop input smoke test failed: result=%+v err=%v", result, err)
	}
}
