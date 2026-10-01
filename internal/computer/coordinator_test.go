package computer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	sequence     atomic.Int64
	resets       atomic.Int64
	acts         atomic.Int64
	entered      chan struct{}
	block        chan struct{}
	fail         bool
	closeEntered chan struct{}
	closeBlock   chan struct{}
	resetFail    atomic.Bool
}

func (*fakeBackend) Targets(context.Context) (TargetsResult, error) { return TargetsResult{}, nil }
func (b *fakeBackend) State(ctx context.Context, options StateOptions) ([]byte, State, error) {
	return []byte{1}, State{StateID: time.Unix(b.sequence.Add(1), 0).String(), Elements: []Element{{RefID: "element"}}}, nil
}
func (b *fakeBackend) Act(ctx context.Context, action Action) (ActionResult, error) {
	b.acts.Add(1)
	if b.entered != nil {
		close(b.entered)
	}
	if b.block != nil {
		select {
		case <-ctx.Done():
			return ActionResult{}, ctx.Err()
		case <-b.block:
		}
	}
	if b.fail {
		return ActionResult{}, errors.New("partial failure")
	}
	return ActionResult{Success: true}, nil
}
func (b *fakeBackend) Reset(context.Context) error {
	b.resets.Add(1)
	if b.resetFail.Load() {
		return errors.New("semantic action still settling")
	}
	return nil
}
func (*fakeBackend) Invalidate(context.Context) error          { return nil }
func (*fakeBackend) ShowControl(context.Context, string) error { return nil }
func (b *fakeBackend) Close() error {
	if b.closeEntered != nil {
		close(b.closeEntered)
	}
	if b.closeBlock != nil {
		<-b.closeBlock
	}
	return nil
}
func acquire(t *testing.T, c *coordinator) string {
	t.Helper()
	status, err := c.Control(context.Background(), ControlOptions{Kind: "acquire", Label: "test"})
	if err != nil || status.ControlID == "" {
		t.Fatalf("acquire: %+v %v", status, err)
	}
	return status.ControlID
}
func observation(t *testing.T, c *coordinator, id string) State {
	t.Helper()
	_, state, err := c.State(context.Background(), StateOptions{ControlID: id})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestControlOwnershipAndReadOnly(t *testing.T) {
	c := newCoordinator(context.Background(), &fakeBackend{})
	defer c.Close()
	id := acquire(t, c)
	if _, err := c.Control(context.Background(), ControlOptions{Kind: "acquire"}); err == nil {
		t.Fatal("second owner accepted")
	}
	status, _ := c.Control(context.Background(), ControlOptions{Kind: "status"})
	if status.ControlID != "" {
		t.Fatal("status leaked owner token")
	}
	_, readOnly, err := c.State(context.Background(), StateOptions{})
	if err != nil || readOnly.Actionable || readOnly.StateID != "" || readOnly.Elements[0].RefID != "" {
		t.Fatalf("read-only: %+v %v", readOnly, err)
	}
	controlled := observation(t, c, id)
	if !controlled.Actionable {
		t.Fatal("controlled observation not actionable")
	}
	if _, err := c.Act(context.Background(), Action{ControlID: "another", Kind: "press_key", StateID: controlled.StateID, Key: "A"}); err == nil {
		t.Fatal("foreign owner accepted")
	}
	if _, err := c.Control(context.Background(), ControlOptions{Kind: "release", ControlID: id}); err != nil {
		t.Fatal(err)
	}
	newID := acquire(t, c)
	if newID == id {
		t.Fatal("control token reused")
	}
	if _, err := c.Act(context.Background(), Action{ControlID: newID, Kind: "press_key", StateID: controlled.StateID, Key: "A"}); err == nil {
		t.Fatal("old owner state accepted")
	}
}
func TestPartialFailureInvalidatesObservation(t *testing.T) {
	b := &fakeBackend{fail: true}
	c := newCoordinator(context.Background(), b)
	defer c.Close()
	id := acquire(t, c)
	state := observation(t, c, id)
	action := Action{ControlID: id, Kind: "press_key", StateID: state.StateID, Key: "A"}
	if _, err := c.Act(context.Background(), action); err == nil {
		t.Fatal("partial failure missing")
	}
	if _, err := c.Act(context.Background(), action); err == nil {
		t.Fatal("partial failure left actionable state")
	}
	if b.acts.Load() != 1 {
		t.Fatal("stale action reached backend")
	}
}
func TestCancelledQueueNeverDispatchesAndStopInterrupts(t *testing.T) {
	b := &fakeBackend{entered: make(chan struct{}), block: make(chan struct{})}
	c := newCoordinator(context.Background(), b)
	defer c.Close()
	id := acquire(t, c)
	state := observation(t, c, id)
	done := make(chan error, 1)
	go func() {
		_, err := c.Act(context.Background(), Action{ControlID: id, Kind: "press_key", StateID: state.StateID, Key: "A"})
		done <- err
	}()
	<-b.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.State(ctx, StateOptions{ControlID: id}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queue: %v", err)
	}
	if _, err := c.Control(context.Background(), ControlOptions{Kind: "acquire"}); err == nil {
		t.Fatal("acquire queued or accepted while active")
	}
	if _, err := c.Control(context.Background(), ControlOptions{Kind: "release", ControlID: id}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop did not cancel active action: %v", err)
	}
	status, _ := c.Control(context.Background(), ControlOptions{Kind: "status"})
	if status.Status != "free" || b.resets.Load() == 0 {
		t.Fatalf("cleanup: %+v", status)
	}
}
func TestIdleExpiryAndRefresh(t *testing.T) {
	c := newCoordinator(context.Background(), &fakeBackend{})
	defer c.Close()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	c.mu.Lock()
	c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	c.mu.Unlock()
	id := acquire(t, c)
	clock.Add(int64(4 * time.Minute))
	_ = observation(t, c, id)
	clock.Add(int64(4 * time.Minute))
	status, _ := c.Control(context.Background(), ControlOptions{Kind: "status"})
	if status.Status != "owned" {
		t.Fatal("active control was not refreshed")
	}
	clock.Add(int64(2 * time.Minute))
	status, _ = c.Control(context.Background(), ControlOptions{Kind: "status"})
	if status.Status != "free" {
		t.Fatalf("idle control retained: %+v", status)
	}
	if _, _, err := c.State(context.Background(), StateOptions{ControlID: id}); err == nil {
		t.Fatal("expired token revived")
	}
}
func TestBrowserBorrowSurvivesRequestCancellationUntilAcknowledged(t *testing.T) {
	c := newCoordinator(context.Background(), &fakeBackend{})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	_, release, err := c.BeginDesktopOperation(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := c.Control(context.Background(), ControlOptions{Kind: "acquire"}); err == nil {
		t.Fatal("cancel released unacknowledged browser operation")
	}
	release()
	id := acquire(t, c)
	if _, _, err := c.BeginDesktopOperation(context.Background(), ""); err == nil {
		t.Fatal("browser borrowed another task's desktop")
	}
	_, release, err = c.BeginDesktopOperation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestConcurrentCloseJoinsCleanup(t *testing.T) {
	b := &fakeBackend{closeEntered: make(chan struct{}), closeBlock: make(chan struct{})}
	c := newCoordinator(context.Background(), b)
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- c.Close() }()
	<-b.closeEntered
	go func() { second <- c.Close() }()
	select {
	case <-second:
		t.Fatal("second Close returned before native cleanup")
	case <-time.After(20 * time.Millisecond):
	}
	close(b.closeBlock)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestSettlingCleanupBlocksNewOwner(t *testing.T) {
	b := &fakeBackend{}
	c := newCoordinator(context.Background(), b)
	defer c.Close()
	id := acquire(t, c)
	b.resetFail.Store(true)
	status, err := c.Control(context.Background(), ControlOptions{Kind: "release", ControlID: id})
	if err != nil || status.Status != "stopping" {
		t.Fatalf("unsafe cleanup status: %+v %v", status, err)
	}
	if _, err := c.Control(context.Background(), ControlOptions{Kind: "acquire"}); err == nil {
		t.Fatal("new owner acquired while old effect still settling")
	}
	b.resetFail.Store(false)
	c.finishStop()
	status, _ = c.Control(context.Background(), ControlOptions{Kind: "status"})
	if status.Status != "free" {
		t.Fatalf("settled cleanup did not become free: %+v", status)
	}
}

func TestLocalStopCancelsBrowserBorrowBeforeRelease(t *testing.T) {
	c := newCoordinator(context.Background(), &fakeBackend{})
	defer c.Close()
	id := acquire(t, c)
	operation, release, err := c.BeginDesktopOperation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	c.LocalStop()
	select {
	case <-operation.Done():
	case <-time.After(time.Second):
		t.Fatal("LocalStop did not cancel shared Browser operation")
	}
	if _, err := c.Control(context.Background(), ControlOptions{Kind: "acquire"}); err == nil {
		t.Fatal("desktop released before Browser acknowledgment")
	}
	release()
}
