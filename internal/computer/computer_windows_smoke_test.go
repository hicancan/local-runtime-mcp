//go:build windows

package computer

import (
	"context"
	"os"
	"testing"
	"time"
)

// The read-only smoke observes the real desktop; all input tests use the owned
// fixture in TestWindowsComputerContractSmoke.
func TestWindowsDesktopSmoke(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_DESKTOP_SMOKE") != "1" {
		t.Skip("set LOCAL_RUNTIME_MCP_DESKTOP_SMOKE=1 in an interactive Windows session")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := New(ctx)
	defer controller.Close()
	targets, err := controller.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var active string
	for _, target := range targets.Windows {
		if target.Active {
			active = target.TargetID
			break
		}
	}
	for _, options := range []StateOptions{{TargetID: active}, {}} {
		var data []byte
		var state State
		for attempt := 0; attempt < 4; attempt++ {
			data, state, err = controller.State(ctx, options)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" || state.Width < 1 || state.Height < 1 {
			t.Fatalf("invalid WGC state: bytes=%d state=%+v", len(data), state)
		}
		if state.Actionable || state.StateID != "" {
			t.Fatalf("read-only observation became actionable: %+v", state)
		}
		for _, element := range state.Elements {
			if element.RefID != "" {
				t.Fatal("read-only element received action reference")
			}
		}
		t.Logf("target=%q WGC=%dx%d UIA=%s elements=%d", state.TargetID, state.Width, state.Height, state.UIAStatus, len(state.Elements))
	}
}
