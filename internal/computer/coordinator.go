package computer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const controlIdleTimeout = 5 * time.Minute

type ControlOptions struct {
	Kind      string `json:"kind" jsonschema:"acquire, status, or release"`
	ControlID string `json:"control_id,omitempty" jsonschema:"opaque control ID required for release"`
	Label     string `json:"label,omitempty" jsonschema:"short task label shown locally while controlling the desktop"`
}

type ControlStatus struct {
	Status    string     `json:"status"`
	ControlID string     `json:"control_id,omitempty"`
	Label     string     `json:"label,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// backend owns native state; coordinator owns desktop workflow control.
type backend interface {
	Targets(context.Context) (TargetsResult, error)
	State(context.Context, StateOptions) ([]byte, State, error)
	Act(context.Context, Action) (ActionResult, error)
	Reset(context.Context) error
	Invalidate(context.Context) error
	ShowControl(context.Context, string) error
	Close() error
}

type coordinator struct {
	ctx               context.Context
	cancel            context.CancelFunc
	native            backend
	gate              chan struct{}
	waiting           chan struct{}
	mu                sync.Mutex
	stopMu            sync.Mutex
	status, id, label string
	expires           time.Time
	now               func() time.Time
	operationCancel   context.CancelFunc
	states            map[string]string
	closed            bool
	closeDone         chan struct{}
	watchDone         chan struct{}
	closeErr          error
}

func newCoordinator(ctx context.Context, native backend) *coordinator {
	ctx, cancel := context.WithCancel(ctx)
	c := &coordinator{ctx: ctx, cancel: cancel, native: native, gate: make(chan struct{}, 1), waiting: make(chan struct{}, 64), status: "free", now: time.Now, states: make(map[string]string), closeDone: make(chan struct{}), watchDone: make(chan struct{})}
	c.gate <- struct{}{}
	go func() {
		defer close(c.watchDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				c.expire()
				c.mu.Lock()
				retry := c.status == "stopping" && !c.closed
				c.mu.Unlock()
				if retry {
					c.finishStop()
				}
			}
		}
	}()
	return c
}

func (c *coordinator) expire() {
	c.mu.Lock()
	should := c.status == "owned" && c.operationCancel == nil && !c.now().Before(c.expires)
	if should {
		c.status = "stopping"
		c.id = ""
		clear(c.states)
	}
	c.mu.Unlock()
	if should {
		c.finishStop()
	}
}

func (c *coordinator) acquireGate(ctx context.Context) error {
	select {
	case c.waiting <- struct{}{}:
	default:
		return errors.New("computer operation queue is full")
	}
	defer func() { <-c.waiting }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("computer controller closed")
	case <-c.gate:
	}
	if err := ctx.Err(); err != nil {
		c.gate <- struct{}{}
		return err
	}
	return nil
}

func (c *coordinator) statusLocked() ControlStatus {
	result := ControlStatus{Status: c.status, Label: c.label}
	if c.status == "owned" {
		expiry := c.expires
		result.ExpiresAt = &expiry
	}
	return result
}

func (c *coordinator) Control(ctx context.Context, options ControlOptions) (ControlStatus, error) {
	c.expire()
	switch options.Kind {
	case "status":
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.statusLocked(), nil
	case "acquire":
		if options.ControlID != "" {
			return ControlStatus{}, errors.New("acquire does not accept control_id")
		}
		if len(options.Label) > 160 {
			return ControlStatus{}, errors.New("label exceeds 160 bytes")
		}
		if err := ctx.Err(); err != nil {
			return ControlStatus{}, err
		}
		select {
		case <-c.gate:
		default:
			return ControlStatus{}, errors.New("desktop busy; retry after the current operation")
		}
		defer func() { c.gate <- struct{}{} }()
		c.mu.Lock()
		if c.closed || c.status != "free" {
			c.mu.Unlock()
			return ControlStatus{}, errors.New("desktop busy; inspect computer_control status")
		}
		var bytes [24]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			c.mu.Unlock()
			return ControlStatus{}, err
		}
		id := hex.EncodeToString(bytes[:])
		c.status, c.id, c.label = "owned", id, options.Label
		c.expires = c.now().Add(controlIdleTimeout)
		c.mu.Unlock()
		if err := c.native.ShowControl(ctx, options.Label); err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			cleanupError := c.native.Reset(cleanup)
			cancel()
			c.mu.Lock()
			c.status, c.id, c.label = "free", "", ""
			if cleanupError != nil {
				c.status = "stopping"
			}
			c.mu.Unlock()
			return ControlStatus{}, err
		}
		c.mu.Lock()
		if c.status != "owned" || c.id != id {
			c.mu.Unlock()
			return ControlStatus{}, errors.New("desktop control was stopped during acquisition")
		}
		result := c.statusLocked()
		result.ControlID = id
		c.mu.Unlock()
		return result, nil
	case "release":
		c.mu.Lock()
		if options.ControlID == "" || c.status != "owned" || options.ControlID != c.id {
			c.mu.Unlock()
			return ControlStatus{}, errors.New("control_id is unknown, expired, or released")
		}
		c.status, c.id = "stopping", ""
		clear(c.states)
		if c.operationCancel != nil {
			c.operationCancel()
		}
		c.mu.Unlock()
		c.finishStop()
		c.mu.Lock()
		result := c.statusLocked()
		c.mu.Unlock()
		return result, nil
	default:
		return ControlStatus{}, fmt.Errorf("unsupported computer control operation %q", options.Kind)
	}
}

func (c *coordinator) finishStop() {
	if !c.stopMu.TryLock() {
		return
	}
	defer c.stopMu.Unlock()
	c.mu.Lock()
	stopping := c.status == "stopping" && !c.closed
	c.mu.Unlock()
	if !stopping {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Reset can interrupt the native operation independently of its work queue.
	if err := c.native.Reset(ctx); err != nil {
		return
	}
	if err := c.acquireGate(ctx); err != nil {
		return
	}
	defer func() { c.gate <- struct{}{} }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.status, c.label = "free", ""
	}
	c.operationCancel = nil
	clear(c.states)
}

func (c *coordinator) LocalStop() {
	c.mu.Lock()
	if c.status != "owned" {
		c.mu.Unlock()
		return
	}
	c.status, c.id = "stopping", ""
	clear(c.states)
	if c.operationCancel != nil {
		c.operationCancel()
	}
	c.mu.Unlock()
	go c.finishStop()
}

func (c *coordinator) begin(ctx context.Context, id string, borrow bool) (context.Context, func(), error) {
	c.expire()
	c.mu.Lock()
	valid := !c.closed && ((borrow && id == "" && c.status == "free") || (id != "" && c.status == "owned" && id == c.id))
	c.mu.Unlock()
	if !valid {
		return nil, nil, errors.New("desktop busy or control_id is unknown, expired, or released")
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := c.acquireGate(operation); err != nil {
		cancel()
		return nil, nil, err
	}
	c.mu.Lock()
	valid = !c.closed && ((borrow && id == "" && c.status == "free") || (id != "" && c.status == "owned" && id == c.id))
	if !valid {
		c.mu.Unlock()
		cancel()
		c.gate <- struct{}{}
		return nil, nil, errors.New("desktop busy or control_id is unknown, expired, or released")
	}
	c.operationCancel = cancel
	if id != "" {
		c.expires = c.now().Add(controlIdleTimeout)
	}
	c.mu.Unlock()
	var once sync.Once
	end := func() {
		once.Do(func() {
			cancel()
			c.mu.Lock()
			c.operationCancel = nil
			if id != "" && c.status == "owned" && id == c.id {
				c.expires = c.now().Add(controlIdleTimeout)
			}
			stopping := c.status == "stopping" && !c.closed
			c.mu.Unlock()
			c.gate <- struct{}{}
			if stopping {
				go c.finishStop()
			}
		})
	}
	return operation, end, nil
}

func (c *coordinator) BeginDesktopOperation(ctx context.Context, id string) (context.Context, func(), error) {
	operation, end, err := c.begin(ctx, id, true)
	if err != nil {
		return nil, nil, err
	}
	// A Browser-visible change invalidates every native observation before it starts.
	if err := c.native.Invalidate(operation); err != nil {
		end()
		return nil, nil, err
	}
	c.mu.Lock()
	clear(c.states)
	c.mu.Unlock()
	return operation, end, nil
}

func (c *coordinator) Targets(ctx context.Context) (TargetsResult, error) {
	return c.native.Targets(ctx)
}
func (c *coordinator) State(ctx context.Context, options StateOptions) ([]byte, State, error) {
	if options.ControlID == "" {
		image, state, err := c.native.State(ctx, options)
		state.Actionable = false
		state.StateID = ""
		for i := range state.Elements {
			state.Elements[i].RefID = ""
		}
		return image, state, err
	}
	operation, end, err := c.begin(ctx, options.ControlID, false)
	if err != nil {
		return nil, State{}, err
	}
	defer end()
	image, state, err := c.native.State(operation, options)
	if err != nil {
		return nil, State{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != "owned" || c.id != options.ControlID {
		return nil, State{}, errors.New("desktop control was revoked during observation")
	}
	state.Actionable = true
	if len(c.states) >= 64 {
		clear(c.states)
	}
	c.states[state.StateID] = options.ControlID
	return image, state, nil
}
func (c *coordinator) Act(ctx context.Context, action Action) (ActionResult, error) {
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	operation, end, err := c.begin(ctx, action.ControlID, false)
	if err != nil {
		return ActionResult{}, err
	}
	defer end()
	c.mu.Lock()
	valid := action.Kind == "activate" || c.states[action.StateID] == action.ControlID
	if valid {
		clear(c.states)
	}
	c.mu.Unlock()
	if !valid {
		return ActionResult{}, errors.New("state_id is unknown or belongs to a previous desktop control; observe again")
	}
	return c.native.Act(operation, action)
}
func (c *coordinator) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
	c.closed = true
	c.status, c.id = "stopping", ""
	clear(c.states)
	if c.operationCancel != nil {
		c.operationCancel()
	}
	c.mu.Unlock()
	c.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.native.Reset(ctx)
	err := c.native.Close()
	<-c.watchDone
	c.mu.Lock()
	c.closeErr = err
	close(c.closeDone)
	c.mu.Unlock()
	return err
}
