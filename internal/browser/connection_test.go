package browser

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestBridgeDisconnectedAuthorizationFailureReclaimsCapacity(t *testing.T) {
	for _, gateError := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled-control", true: "gate-error"}[gateError], func(t *testing.T) {
			b, _ := testBridge(t)
			a := peer("profile", "boot")
			register(t, b, a)
			if err := b.registerTabs(a.InstanceID, []Tab{{ID: "boot.tab"}}); err != nil {
				t.Fatal(err)
			}
			entered, proceed := make(chan struct{}), make(chan struct{})
			released := 0
			b.SetDesktopGate(func(ctx context.Context, _ string) (context.Context, func(), error) {
				close(entered)
				<-proceed
				if gateError {
					return nil, nil, errors.New("control unavailable")
				}
				return ctx, func() { released++ }, nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := b.Act(context.Background(), Action{TabID: "boot.tab", Kind: "evaluate", Script: "1"})
				done <- err
			}()
			item := pollCommand(t, b, a)
			authorized := make(chan int, 1)
			go func() {
				r, err := postBridge(context.Background(), b, "authorize", response{ID: item.ID, InstanceID: a.InstanceID, BootID: a.BootID})
				if err != nil {
					authorized <- 0
					return
				}
				r.Body.Close()
				authorized <- r.StatusCode
			}()
			<-entered
			_ = testSocket(b, a).Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("disconnected action did not cancel")
			}
			close(proceed)
			if status := <-authorized; status != 409 {
				t.Fatal(status)
			}
			b.mu.Lock()
			pending := len(b.pending)
			b.mu.Unlock()
			if pending != 0 {
				t.Fatal("failed authorization retained pending capacity")
			}
			if !gateError && released != 1 {
				t.Fatal("late authorization reservation was not released")
			}
		})
	}
}

func testSocket(b *Bridge, p Peer) *websocket.Conn {
	value, _ := testConnections.Load(testConnectionKey{b, p.InstanceID, p.BootID})
	return value.(*websocket.Conn)
}

func TestBridgeDisconnectReclaimsUngatedCapacity(t *testing.T) {
	b, ctx := testBridge(t)
	a := peer("offline", "boot")
	register(t, b, a)
	done := make(chan error, 1)
	go func() { _, err := b.Tabs(ctx, a.InstanceID); done <- err }()
	_ = pollCommand(t, b, a)
	_ = testSocket(b, a).Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnected call succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnected call did not settle")
	}
	b.mu.Lock()
	pending := len(b.pending)
	b.mu.Unlock()
	if pending != 0 {
		t.Fatalf("ungated disconnected call retained capacity: %d", pending)
	}
	c := peer("online", "boot-online")
	register(t, b, c)
	go func() { _, err := b.Tabs(ctx, c.InstanceID); done <- err }()
	item := pollCommand(t, b, c)
	if finishCommand(t, b, c, item, []Tab{{ID: c.BootID + ".tab"}}) != http.StatusNoContent {
		t.Fatal("new profile failed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBridgeReconnectPreservesExecutionAcknowledgement(t *testing.T) {
	b, _ := testBridge(t)
	a := peer("profile", "boot")
	register(t, b, a)
	if err := b.registerTabs(a.InstanceID, []Tab{{ID: "boot.old"}}); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{}, 2)
	b.SetDesktopGate(func(ctx context.Context, _ string) (context.Context, func(), error) {
		return ctx, func() { released <- struct{}{} }, nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := b.Act(context.Background(), Action{TabID: "boot.old", Kind: "evaluate", Script: "1"})
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
	oldSocket := testSocket(b, a)
	_ = oldSocket.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("call did not cancel")
	}
	select {
	case <-released:
		t.Fatal("disconnection released desktop before acknowledgement")
	default:
	}
	register(t, b, a)
	_ = oldSocket.Close()
	if !b.Status().Connected {
		t.Fatal("old socket close disconnected its replacement")
	}
	if _, err := b.route("boot.old"); err == nil {
		t.Fatal("old handle survived stream replacement")
	}
	select {
	case <-released:
		t.Fatal("reconnection released desktop before acknowledgement")
	default:
	}
	if status := finishCommand(t, b, a, item, ActionResult{Success: true}); status != 204 {
		t.Fatal(status)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("ack did not release desktop")
	}
	select {
	case <-released:
		t.Fatal("desktop released twice")
	default:
	}
	b.mu.Lock()
	pending := len(b.pending)
	b.mu.Unlock()
	if pending != 0 {
		t.Fatal("ack retained pending call")
	}
}

func TestBridgeShutdownJoinsAuthenticatedAndUnauthenticatedStreams(t *testing.T) {
	b, _ := testBridge(t)
	register(t, b, peer("active", "boot"))
	unauthed, err := websocket.Dial("ws://"+b.Status().Address+"/v1/connect", "", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	defer unauthed.Close()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		n := len(b.sockets)
		b.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second stream not registered")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- b.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for authentication timeout")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sockets) != 0 {
		t.Fatal("shutdown retained hijacked streams")
	}
}
