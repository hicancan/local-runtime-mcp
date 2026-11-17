package browser

import (
	"context"
	"testing"
)

func TestCloseConfirmsAlreadyAbsentTabButPreservesRealFailures(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "already_closed", true: "still_present"}[present], func(t *testing.T) {
			b, ctx := testBridge(t)
			p := peer("owned", "generation")
			register(t, b, p)
			id := "generation.target"
			b.mu.Lock()
			b.tabs[id] = tabRoute{p.InstanceID, p.BootID}
			b.mu.Unlock()
			done := make(chan error, 1)
			go func() { done <- b.CloseTab(ctx, id, "") }()
			closeCall := pollCommand(t, b, p)
			if closeCall.Method != "tabs.close" {
				t.Fatal(closeCall.Method)
			}
			reply, err := postBridge(context.Background(), b, "result", response{ID: closeCall.ID, InstanceID: p.InstanceID, BootID: p.BootID, Error: "synthetic browser refusal"})
			if err != nil {
				t.Fatal(err)
			}
			reply.Body.Close()
			observe := pollCommand(t, b, p)
			if observe.Method != "tabs.list" {
				t.Fatal(observe.Method)
			}
			tabs := []Tab{{ID: "generation.other"}}
			if present {
				tabs = append(tabs, Tab{ID: id})
			}
			finishCommand(t, b, p, observe, tabs)
			result := <-done
			if present && result == nil {
				t.Fatal("a present tab's close failure was hidden")
			}
			if !present && result != nil {
				t.Fatal(result)
			}
			b.mu.Lock()
			_, retained := b.tabs[id]
			_, other := b.tabs["generation.other"]
			b.mu.Unlock()
			if retained != present || !other {
				t.Fatal("wrong tab routing was removed")
			}
		})
	}
}
