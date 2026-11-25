package jsrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
)

const (
	maxLiveSessions = 64
	sessionIdleTTL  = 30 * time.Minute
)

// Dependencies supplies the underlying native machine subsystems to the JS runtime.
type Dependencies struct {
	Process      *runtimeprocess.Manager
	Filesystem   *filesystem.Service
	Browser      *browser.Bridge
	Computer     computer.Controller
	Capabilities *capability.Registry
	LocalApps    bool
}

// Engine manages per-session persistent JavaScript VMs and async cells.
type Engine struct {
	ctx    context.Context
	cancel context.CancelFunc
	deps   Dependencies

	mu       sync.Mutex
	closed   bool
	sessions map[string]*sessionVM
	cells    map[string]*cellEntry
}

type cellEntry struct {
	cell *cellRun
	vm   *sessionVM
}

func NewEngine(parent context.Context, deps Dependencies) *Engine {
	ctx, cancel := context.WithCancel(parent)
	e := &Engine{
		ctx:      ctx,
		cancel:   cancel,
		deps:     deps,
		sessions: map[string]*sessionVM{},
		cells:    map[string]*cellEntry{},
	}
	go e.reaperLoop()
	return e
}

func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	vms := make([]*sessionVM, 0, len(e.sessions))
	for _, vm := range e.sessions {
		vms = append(vms, vm)
	}
	e.sessions = map[string]*sessionVM{}
	e.mu.Unlock()

	e.cancel()
	for _, vm := range vms {
		vm.close()
	}
}

// CloseSession terminates the persistent JS VM and cleans up temporary browser
// tabs owned by sessionID.
func (e *Engine) CloseSession(ctx context.Context, sessionID string) {
	if e == nil || sessionID == "" {
		return
	}
	e.mu.Lock()
	vm := e.sessions[sessionID]
	delete(e.sessions, sessionID)
	e.mu.Unlock()
	if vm != nil {
		vm.close()
	}
	if e.deps.Browser != nil {
		e.deps.Browser.CleanupSessionTabs(ctx, sessionID)
	}
}

func (e *Engine) reaperLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			var evict []*sessionVM
			e.mu.Lock()
			for sid, vm := range e.sessions {
				if vm.isIdle(now, sessionIdleTTL) {
					delete(e.sessions, sid)
					evict = append(evict, vm)
				}
			}
			for cid, entry := range e.cells {
				entry.cell.mu.Lock()
				done := entry.cell.status != "running" && !entry.cell.finishedAt.IsZero() && now.Sub(entry.cell.finishedAt) > cellRetention
				entry.cell.mu.Unlock()
				if done {
					delete(e.cells, cid)
				}
			}
			e.mu.Unlock()
			for _, vm := range evict {
				vm.close()
			}
		case <-e.ctx.Done():
			return
		}
	}
}

func (e *Engine) getOrCreateVM(sessionID string) (*sessionVM, error) {
	if sessionID == "" {
		sessionID = "default"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("runtime engine is closed")
	}
	if vm := e.sessions[sessionID]; vm != nil {
		vm.touch()
		return vm, nil
	}
	if len(e.sessions) >= maxLiveSessions {
		// Evict oldest idle session.
		var oldestID string
		var oldestTime time.Time
		for sid, vm := range e.sessions {
			t := vm.lastUsedTime()
			if oldestID == "" || t.Before(oldestTime) {
				oldestID = sid
				oldestTime = t
			}
		}
		if oldestID != "" {
			old := e.sessions[oldestID]
			delete(e.sessions, oldestID)
			go old.close()
		}
	}
	vm := newSessionVM(e.ctx, sessionID, e.deps)
	e.sessions[sessionID] = vm
	return vm, nil
}

// Run executes JavaScript code inside the session's persistent stateful VM,
// waiting up to YieldMS before yielding an async cell_id.
func (e *Engine) Run(ctx context.Context, opts RunOptions) (CellResult, error) {
	if strings.TrimSpace(opts.Title) == "" {
		return CellResult{}, errors.New("title is required")
	}
	if strings.TrimSpace(opts.Code) == "" {
		return CellResult{}, errors.New("code is required")
	}
	yieldMS := opts.YieldMS
	if yieldMS <= 0 {
		yieldMS = defaultYieldMS
	}
	if yieldMS > maxYieldMS {
		yieldMS = maxYieldMS
	}

	vm, err := e.getOrCreateVM(opts.SessionID)
	if err != nil {
		return CellResult{}, err
	}

	cell := newCellRun(e.ctx, opts)
	e.mu.Lock()
	e.cells[cell.id] = &cellEntry{cell: cell, vm: vm}
	e.mu.Unlock()

	if err := vm.startCell(cell, opts.Code); err != nil {
		cell.finish("error", nil, err.Error())
		return cell.snapshot(false), nil
	}

	timer := time.NewTimer(time.Duration(yieldMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-cell.done:
		return cell.snapshot(false), nil
	case <-timer.C:
		return cell.snapshot(true), nil
	case <-ctx.Done():
		cell.finish("canceled", nil, ctx.Err().Error())
		return cell.snapshot(false), ctx.Err()
	}
}

// Wait awaits, reads incremental output from, or terminates an active or recently completed cell.
func (e *Engine) Wait(ctx context.Context, opts WaitOptions) (CellResult, error) {
	if strings.TrimSpace(opts.CellID) == "" {
		return CellResult{}, errors.New("cell_id is required")
	}
	e.mu.Lock()
	entry := e.cells[opts.CellID]
	e.mu.Unlock()
	if entry == nil {
		return CellResult{}, fmt.Errorf("cell %q was not found or has expired", opts.CellID)
	}
	cell := entry.cell
	entry.vm.touch()

	if opts.Terminate {
		cell.finish("canceled", nil, "terminated by caller")
		return cell.snapshot(true), nil
	}

	yieldMS := opts.YieldMS
	if yieldMS <= 0 {
		yieldMS = defaultYieldMS
	}
	if yieldMS > maxYieldMS {
		yieldMS = maxYieldMS
	}

	select {
	case <-cell.done:
		return cell.snapshot(true), nil
	default:
	}

	timer := time.NewTimer(time.Duration(yieldMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-cell.done:
		return cell.snapshot(true), nil
	case <-timer.C:
		return cell.snapshot(true), nil
	case <-ctx.Done():
		return cell.snapshot(true), ctx.Err()
	}
}

// sessionVM owns one goja.Runtime and its single-threaded event loop goroutine.
type sessionVM struct {
	sessionID string
	deps      Dependencies
	vm        *goja.Runtime

	// activeCell tracks the currently executing async cell on the VM goroutine
	// (Node.js AsyncLocalStorage semantics) so helper functions defined in turn 1
	// and called in turn 2 always bind to turn 2's live cell context and output.
	activeCell *cellRun

	ctx    context.Context
	cancel context.CancelFunc
	tasks  chan func()
	done   chan struct{}

	mu          sync.Mutex
	closed      bool
	lastUsed    time.Time
	activeCells int
}

func newSessionVM(parent context.Context, sessionID string, deps Dependencies) *sessionVM {
	ctx, cancel := context.WithCancel(parent)
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	s := &sessionVM{
		sessionID: sessionID,
		deps:      deps,
		vm:        vm,
		ctx:       ctx,
		cancel:    cancel,
		tasks:     make(chan func(), 256),
		done:      make(chan struct{}),
		lastUsed:  time.Now(),
	}
	s.installGlobals()
	go s.loop()
	return s
}

func (v *sessionVM) currentCell() *cellRun {
	return v.activeCell
}

func (v *sessionVM) touch() {
	v.mu.Lock()
	v.lastUsed = time.Now()
	v.mu.Unlock()
}

func (v *sessionVM) lastUsedTime() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastUsed
}

func (v *sessionVM) isIdle(now time.Time, ttl time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.activeCells == 0 && now.Sub(v.lastUsed) > ttl
}

func (v *sessionVM) close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.mu.Unlock()
	v.cancel()
	v.vm.Interrupt("session closed")
	<-v.done
}

func (v *sessionVM) post(fn func()) bool {
	select {
	case v.tasks <- fn:
		return true
	case <-v.ctx.Done():
		return false
	}
}

func (v *sessionVM) loop() {
	defer close(v.done)
	for {
		select {
		case fn := <-v.tasks:
			if fn != nil {
				func() {
					defer func() { _ = recover() }()
					fn()
				}()
			}
		case <-v.ctx.Done():
			return
		}
	}
}

func (v *sessionVM) startCell(cell *cellRun, code string) error {
	wrapped := wrapReplCode(code)
	prog, err := goja.Compile("cell.js", wrapped, false)
	if err != nil {
		return fmt.Errorf("JavaScript syntax error: %w", err)
	}

	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return errors.New("session VM is closed")
	}
	v.activeCells++
	v.lastUsed = time.Now()
	v.mu.Unlock()

	decActive := func() {
		v.mu.Lock()
		if v.activeCells > 0 {
			v.activeCells--
		}
		v.lastUsed = time.Now()
		v.mu.Unlock()
	}

	ok := v.post(func() {
		prev := v.activeCell
		v.activeCell = cell
		defer func() { v.activeCell = prev }()

		val, runErr := v.vm.RunProgram(prog)
		if runErr != nil {
			decActive()
			cell.finish("error", nil, runErr.Error())
			return
		}
		fn, callable := goja.AssertFunction(val)
		if !callable {
			decActive()
			cell.finish("error", nil, "internal error: compiled wrapper is not callable")
			return
		}
		res, callErr := fn(goja.Undefined())
		if callErr != nil {
			decActive()
			cell.finish("error", nil, callErr.Error())
			return
		}

		// Attach .then(resolve, reject) to the async IIFE Promise.
		if obj := res.ToObject(v.vm); obj != nil {
			if thenVal := obj.Get("then"); thenVal != nil {
				if thenFn, ok := goja.AssertFunction(thenVal); ok {
					onFulfilled := v.vm.ToValue(func(fc goja.FunctionCall) goja.Value {
						var out any
						if len(fc.Arguments) > 0 && !goja.IsUndefined(fc.Arguments[0]) {
							out = safeExportValue(fc.Arguments[0])
						}
						decActive()
						cell.finish("done", out, "")
						return goja.Undefined()
					})
					onRejected := v.vm.ToValue(func(fc goja.FunctionCall) goja.Value {
						msg := "unhandled promise rejection"
						if len(fc.Arguments) > 0 && !goja.IsUndefined(fc.Arguments[0]) {
							msg = fc.Arguments[0].String()
						}
						decActive()
						cell.finish("error", nil, msg)
						return goja.Undefined()
					})
					_, _ = thenFn(obj, onFulfilled, onRejected)
					return
				}
			}
		}
		decActive()
		cell.finish("done", safeExportValue(res), "")
	})
	if !ok {
		decActive()
		return errors.New("session VM is closed")
	}
	return nil
}

func (v *sessionVM) spawnAsync(opName, target string, fn func(context.Context) (any, error), convert ...func(any) goja.Value) goja.Value {
	cell := v.currentCell()
	if cell == nil {
		return v.rejected(errors.New("no active execution cell"))
	}
	p, resolve, reject := v.vm.NewPromise()
	go func() {
		start := time.Now()
		res, err := fn(cell.ctx)
		dur := time.Since(start).Milliseconds()
		cell.recordSubOp(opName, target, dur, err)
		v.post(func() {
			prev := v.activeCell
			v.activeCell = cell
			defer func() { v.activeCell = prev }()
			if err != nil {
				_ = reject(v.vm.NewGoError(err))
				return
			}
			if len(convert) > 0 && convert[0] != nil {
				_ = resolve(convert[0](res))
				return
			}
			_ = resolve(v.vm.ToValue(res))
		})
	}()
	return v.vm.ToValue(p)
}

func (v *sessionVM) rejected(err error) goja.Value {
	p, _, reject := v.vm.NewPromise()
	_ = reject(v.vm.NewGoError(err))
	return v.vm.ToValue(p)
}

func safeExportValue(val goja.Value) any {
	if val == nil || goja.IsUndefined(val) {
		return nil
	}
	if goja.IsNull(val) {
		return nil
	}
	exp := val.Export()
	b, err := json.Marshal(exp)
	if err != nil {
		return val.String()
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return val.String()
	}
	return out
}
