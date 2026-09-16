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
	if os.Getenv("LOCAL_RUNTIME_MCP_DESKTOP_SMOKE") != "1" {
		t.Skip("set LOCAL_RUNTIME_MCP_DESKTOP_SMOKE=1 in an interactive Windows session")
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
	var activeBounds Rectangle
	for _, target := range targets.Windows {
		if target.Active {
			active = target.TargetID
			activeBounds = target.Bounds
			break
		}
	}
	if active != "" {
		activated, err := controller.Act(context.Background(), Action{Kind: "activate", TargetID: active})
		if err != nil || !activated.Success {
			t.Fatalf("activate current foreground window failed: result=%+v err=%v", activated, err)
		}
		afterActivation, err := controller.Targets(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var preserved bool
		for _, target := range afterActivation.Windows {
			if target.TargetID == active {
				preserved = target.Active && target.Bounds == activeBounds
				break
			}
		}
		if !preserved {
			t.Fatalf("activate changed the foreground target state: target=%s bounds=%+v after=%+v", active, activeBounds, afterActivation.Windows)
		}
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
