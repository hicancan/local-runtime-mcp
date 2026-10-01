package browser

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
	"golang.org/x/net/websocket"
)

const ExtensionVersion = "10.0.1"

// DesktopGate reserves the shared interactive desktop for a browser operation.
// A successful reservation is released only once execution has acknowledged completion.
type DesktopGate func(context.Context, string) (context.Context, func(), error)

type Bridge struct {
	configured bool
	address    string
	token      string
	server     *http.Server
	mu         sync.Mutex
	pending    map[string]*pendingCall
	instances  map[string]*instance
	tabs       map[string]tabRoute
	gate       DesktopGate
	ctx        context.Context
	cancel     context.CancelFunc
	sequence   atomic.Uint64
	prefix     string
	closeOnce  sync.Once
	closeErr   error
	sockets    map[*websocket.Conn]struct{}
	socketWG   sync.WaitGroup
}

type instance struct {
	peer     Peer
	lastSeen time.Time
	commands chan command
	controls chan command
	socket   *websocket.Conn
}

type tabRoute struct{ browserID, bootID string }
type pendingCall struct {
	result                       chan response
	browserID, bootID, controlID string
	ctx                          context.Context
	cancel                       context.CancelFunc
	release                      func()
	authorizing                  bool
	stopGateWatch                func() bool
	cancelSent                   bool
	dispatched, abandoned        bool
}

type Peer struct {
	InstanceID       string `json:"instance_id,omitempty"`
	BootID           string `json:"boot_id"`
	Label            string `json:"label"`
	Browser          string `json:"browser,omitempty"`
	ExtensionVersion string `json:"extension_version,omitempty"`
}

type Status struct {
	Configured bool             `json:"configured"`
	Connected  bool             `json:"connected"`
	Address    string           `json:"address,omitempty"`
	Instances  []InstanceStatus `json:"instances"`
}

type InstanceStatus struct {
	BrowserID        string    `json:"browser_id"`
	Label            string    `json:"label"`
	Browser          string    `json:"browser"`
	ExtensionVersion string    `json:"extension_version"`
	Connected        bool      `json:"connected"`
	LastSeen         time.Time `json:"last_seen"`
}

type Tab struct {
	ID        string `json:"id"`
	BrowserID string `json:"browser_id"`
	WindowID  int    `json:"window_id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Active    bool   `json:"active"`
	Status    string `json:"status,omitempty"`
}

type SnapshotElement struct {
	Ref      string `json:"ref"`
	Role     string `json:"role"`
	Name     string `json:"name,omitempty"`
	Value    string `json:"value,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type Snapshot struct {
	TabID             string            `json:"tab_id"`
	PageEpoch         string            `json:"page_epoch"`
	SnapshotID        string            `json:"snapshot_id"`
	Title             string            `json:"title"`
	URL               string            `json:"url"`
	Text              string            `json:"text"`
	TextTruncated     bool              `json:"text_truncated"`
	Elements          []SnapshotElement `json:"elements"`
	ElementsTruncated bool              `json:"elements_truncated"`
}

type ScreenshotOptions struct {
	TabID    string `json:"tab_id" jsonschema:"opaque tab handle returned by browser_tabs or browser_open"`
	FullPage bool   `json:"full_page,omitempty" jsonschema:"capture the entire scrollable page instead of the viewport"`
	ClipX    int    `json:"clip_x,omitempty" jsonschema:"non-negative page X coordinate for a clipped capture"`
	ClipY    int    `json:"clip_y,omitempty" jsonschema:"non-negative page Y coordinate for a clipped capture"`
	ClipW    int    `json:"clip_width,omitempty" jsonschema:"positive clipped-capture width; requires clip_height"`
	ClipH    int    `json:"clip_height,omitempty" jsonschema:"positive clipped-capture height; requires clip_width"`
}

type ScreenshotInfo struct {
	TabID        string `json:"tab_id"`
	PageEpoch    string `json:"page_epoch"`
	ScreenshotID string `json:"screenshot_id"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	X            int    `json:"x"`
	Y            int    `json:"y"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FullPage     bool   `json:"full_page"`
	MIMEType     string `json:"mime_type"`
}

type Action struct {
	Kind         string   `json:"kind" jsonschema:"browser operation to perform"`
	TabID        string   `json:"tab_id" jsonschema:"opaque tab handle returned by browser_tabs or browser_open"`
	ControlID    string   `json:"control_id,omitempty" jsonschema:"desktop control ID when modifying a currently visible page"`
	Ref          string   `json:"ref,omitempty" jsonschema:"versioned element reference from a retained browser_snapshot in the current page epoch"`
	ScreenshotID string   `json:"screenshot_id,omitempty" jsonschema:"viewport screenshot ID required for coordinate targeting"`
	X            *float64 `json:"x,omitempty" jsonschema:"viewport X coordinate associated with screenshot_id"`
	Y            *float64 `json:"y,omitempty" jsonschema:"viewport Y coordinate associated with screenshot_id"`
	ToX          *float64 `json:"to_x,omitempty" jsonschema:"drag destination viewport X coordinate"`
	ToY          *float64 `json:"to_y,omitempty" jsonschema:"drag destination viewport Y coordinate"`
	Button       string   `json:"button,omitempty" jsonschema:"mouse button; defaults to left"`
	Text         string   `json:"text,omitempty" jsonschema:"text for type_text or set_value"`
	Key          string   `json:"key,omitempty" jsonschema:"key or modifier combination such as Control+L"`
	ScrollX      int      `json:"scroll_x,omitempty" jsonschema:"horizontal wheel delta; positive scrolls right"`
	ScrollY      int      `json:"scroll_y,omitempty" jsonschema:"vertical wheel delta; positive scrolls down"`
	Option       string   `json:"option,omitempty" jsonschema:"select option value, visible text, or label"`
	Checked      *bool    `json:"checked,omitempty" jsonschema:"desired checkbox or radio state"`
	Files        []string `json:"files,omitempty" jsonschema:"absolute machine paths assigned to a file input"`
	Accept       *bool    `json:"accept,omitempty" jsonschema:"whether to accept an open JavaScript dialog"`
	PromptText   string   `json:"prompt_text,omitempty" jsonschema:"text supplied to an accepted prompt dialog"`
	Script       string   `json:"script,omitempty" jsonschema:"JavaScript expression evaluated in the page main world"`
}

type Navigation struct {
	TabID     string `json:"tab_id" jsonschema:"opaque tab handle returned by browser_tabs or browser_open"`
	ControlID string `json:"control_id,omitempty" jsonschema:"desktop control ID when navigating a currently visible page"`
	Kind      string `json:"kind" jsonschema:"navigation operation: url, back, forward, or reload"`
	URL       string `json:"url,omitempty" jsonschema:"absolute URL required when kind is url"`
}

type ActionResult struct {
	TabID   string `json:"tab_id"`
	Kind    string `json:"kind"`
	Success bool   `json:"success"`
	Value   any    `json:"value,omitempty"`
}

type command struct {
	ID       string          `json:"id"`
	BootID   string          `json:"boot_id"`
	Deadline int64           `json:"deadline"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID         string          `json:"id"`
	InstanceID string          `json:"instance_id"`
	BootID     string          `json:"boot_id"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

func Start(ctx context.Context, configuration config.Browser) (*Bridge, error) {
	bridgeContext, cancel := context.WithCancel(ctx)
	bridge := &Bridge{configured: configuration.Token != "", token: configuration.Token, pending: make(map[string]*pendingCall), instances: make(map[string]*instance), tabs: make(map[string]tabRoute), sockets: make(map[*websocket.Conn]struct{}), ctx: bridgeContext, cancel: cancel, prefix: rand.Text()}
	if !bridge.configured {
		return bridge, nil
	}
	if configuration.Listen == "" {
		configuration.Listen = config.DefaultBrowser
	}
	listener, err := net.Listen("tcp", configuration.Listen)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start browser extension bridge: %w", err)
	}
	bridge.address = listener.Addr().String()
	mux := http.NewServeMux()
	mux.Handle("/v1/connect", websocket.Server{Handshake: bridge.socketHandshake, Handler: bridge.connect})
	mux.HandleFunc("/v1/result", bridge.receiveResult)
	mux.HandleFunc("/v1/authorize", bridge.authorizeDesktop)
	bridge.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-bridgeContext.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bridge.Close(shutdownContext)
	}()
	go func() { _ = bridge.server.Serve(listener) }()
	return bridge, nil
}

func (b *Bridge) Close(ctx context.Context) error {
	b.closeOnce.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		b.mu.Lock()
		sockets := make([]*websocket.Conn, 0, len(b.sockets))
		for socket := range b.sockets {
			sockets = append(sockets, socket)
		}
		for id, pending := range b.pending {
			if pending.stopGateWatch != nil {
				pending.stopGateWatch()
			}
			pending.cancel()
			if pending.release != nil {
				pending.release()
			}
			delete(b.pending, id)
		}
		b.mu.Unlock()
		for _, socket := range sockets {
			_ = socket.SetDeadline(time.Now())
			_ = socket.Close()
		}
		if b.server == nil {
			return
		}
		shutdownContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		b.closeErr = b.server.Shutdown(shutdownContext)
		if errors.Is(b.closeErr, context.DeadlineExceeded) {
			b.closeErr = b.server.Close()
		}
		b.socketWG.Wait()
	})
	return b.closeErr
}

func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	status := Status{Configured: b.configured, Address: b.address, Instances: make([]InstanceStatus, 0, len(b.instances))}
	for id, instance := range b.instances {
		connected := instance.socket != nil && time.Since(instance.lastSeen) < 35*time.Second
		status.Connected = status.Connected || connected
		status.Instances = append(status.Instances, InstanceStatus{BrowserID: id, Label: instance.peer.Label, Browser: instance.peer.Browser, ExtensionVersion: instance.peer.ExtensionVersion, Connected: connected, LastSeen: instance.lastSeen})
	}
	sort.Slice(status.Instances, func(i, j int) bool { return status.Instances[i].BrowserID < status.Instances[j].BrowserID })
	return status
}

func (b *Bridge) SetDesktopGate(gate DesktopGate) { b.mu.Lock(); b.gate = gate; b.mu.Unlock() }

func (b *Bridge) Tabs(ctx context.Context, browserID string) ([]Tab, error) {
	var result []Tab
	err := b.call(ctx, browserID, "tabs.list", struct{}{}, "", &result)
	if err == nil {
		err = b.registerTabs(browserID, result)
	}
	return result, err
}

func (b *Bridge) Open(ctx context.Context, browserID, url string, active bool, controlID string) (Tab, error) {
	var result Tab
	err := b.call(ctx, browserID, "tabs.open", map[string]any{"url": url, "active": active}, controlID, &result)
	if err == nil {
		err = b.registerTabs(browserID, []Tab{result})
		result.BrowserID = browserID
	}
	return result, err
}

func (b *Bridge) CloseTab(ctx context.Context, tabID, controlID string) error {
	browserID, err := b.route(tabID)
	if err != nil {
		return err
	}
	err = b.call(ctx, browserID, "tabs.close", map[string]any{"tab_id": tabID}, controlID, &struct{}{})
	if err == nil {
		b.mu.Lock()
		delete(b.tabs, tabID)
		b.mu.Unlock()
	}
	return err
}

func (b *Bridge) Navigate(ctx context.Context, navigation Navigation) (Tab, error) {
	var result Tab
	if navigation.TabID == "" {
		return result, errors.New("tab_id is required")
	}
	switch navigation.Kind {
	case "url":
		if strings.TrimSpace(navigation.URL) == "" {
			return result, errors.New("url navigation requires url")
		}
	case "back", "forward", "reload":
		if navigation.URL != "" {
			return result, errors.New("url is only valid when navigation kind is url")
		}
	default:
		return result, fmt.Errorf("unsupported browser navigation %q", navigation.Kind)
	}
	browserID, err := b.route(navigation.TabID)
	if err != nil {
		return result, err
	}
	err = b.call(ctx, browserID, "page.navigate", navigation, navigation.ControlID, &result)
	result.BrowserID = browserID
	return result, err
}

func (b *Bridge) Snapshot(ctx context.Context, tabID string, maxElements, maxText int) (Snapshot, error) {
	var result Snapshot
	browserID, err := b.route(tabID)
	if err != nil {
		return result, err
	}
	err = b.call(ctx, browserID, "page.snapshot", map[string]any{"tab_id": tabID, "max_elements": maxElements, "max_text": maxText}, "", &result)
	return result, err
}

func (b *Bridge) Screenshot(ctx context.Context, options ScreenshotOptions) ([]byte, ScreenshotInfo, error) {
	var result struct {
		Data string         `json:"data_base64"`
		Info ScreenshotInfo `json:"info"`
	}
	if options.TabID == "" {
		return nil, ScreenshotInfo{}, errors.New("tab_id is required")
	}
	if options.FullPage && (options.ClipW != 0 || options.ClipH != 0 || options.ClipX != 0 || options.ClipY != 0) {
		return nil, ScreenshotInfo{}, errors.New("full_page and clip fields are mutually exclusive")
	}
	if (options.ClipW == 0) != (options.ClipH == 0) || options.ClipW < 0 || options.ClipH < 0 || options.ClipX < 0 || options.ClipY < 0 {
		return nil, ScreenshotInfo{}, errors.New("clip requires positive clip_width and clip_height with non-negative coordinates")
	}
	browserID, err := b.route(options.TabID)
	if err != nil {
		return nil, ScreenshotInfo{}, err
	}
	if err := b.call(ctx, browserID, "page.screenshot", options, "", &result); err != nil {
		return nil, ScreenshotInfo{}, err
	}
	data, err := decodeBase64(result.Data)
	return data, result.Info, err
}

func (b *Bridge) Act(ctx context.Context, action Action) (ActionResult, error) {
	if err := validateAction(action); err != nil {
		return ActionResult{}, err
	}
	var result ActionResult
	browserID, err := b.route(action.TabID)
	if err != nil {
		return result, err
	}
	err = b.call(ctx, browserID, "page.action", action, action.ControlID, &result)
	return result, err
}

func (b *Bridge) route(tabID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	route, ok := b.tabs[tabID]
	if !ok {
		return "", errors.New("tab_id is unknown or stale; call browser_tabs again")
	}
	instance := b.instances[route.browserID]
	if instance == nil || instance.socket == nil || instance.peer.BootID != route.bootID || time.Since(instance.lastSeen) >= 35*time.Second {
		return "", errors.New("browser instance is offline or restarted; call browser_tabs again")
	}
	return route.browserID, nil
}

func (b *Bridge) registerTabs(browserID string, tabs []Tab) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	instance := b.instances[browserID]
	if instance == nil {
		return errors.New("browser instance is offline")
	}
	for i := range tabs {
		if tabs[i].ID == "" {
			return errors.New("extension returned an empty tab handle")
		}
		if !strings.HasPrefix(tabs[i].ID, instance.peer.BootID+".") {
			return errors.New("extension returned a stale-generation tab handle")
		}
		if existing, ok := b.tabs[tabs[i].ID]; ok && (existing.browserID != browserID || existing.bootID != instance.peer.BootID) {
			return errors.New("extension returned a colliding tab handle")
		}
		b.tabs[tabs[i].ID] = tabRoute{browserID, instance.peer.BootID}
		tabs[i].BrowserID = browserID
	}
	return nil
}

func (b *Bridge) call(ctx context.Context, browserID, method string, params any, controlID string, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.configured {
		return errors.New("browser is not configured; run lrmcp setup browser")
	}
	callContext, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	b.mu.Lock()
	instance := b.instances[browserID]
	if instance == nil || instance.socket == nil || time.Since(instance.lastSeen) >= 35*time.Second {
		b.mu.Unlock()
		return errors.New("browser_id is unknown or offline; call browser_status")
	}
	if len(b.pending) >= 256 {
		b.mu.Unlock()
		return errors.New("browser command capacity reached")
	}
	id := fmt.Sprintf("%s-%d", b.prefix, b.sequence.Add(1))
	resultChannel := make(chan response, 1)
	operationContext, operationCancel := context.WithCancel(b.ctx)
	pending := &pendingCall{result: resultChannel, browserID: browserID, bootID: instance.peer.BootID, controlID: controlID, ctx: operationContext, cancel: operationCancel}
	b.pending[id] = pending
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.pending[id] == pending {
			pending.abandoned = true
			b.cancelPendingLocked(id, pending)
		}
		b.mu.Unlock()
	}()
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	deadline, _ := callContext.Deadline()
	select {
	case instance.commands <- command{ID: id, BootID: pending.bootID, Deadline: deadline.UnixMilli(), Method: method, Params: payload}:
	case <-callContext.Done():
		return callContext.Err()
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
	decodeResponse := func(response response) error {
		if response.Error != "" {
			return errors.New(response.Error)
		}
		if output == nil || len(response.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.Result, output); err != nil {
			return fmt.Errorf("decode browser extension response: %w", err)
		}
		return nil
	}
	select {
	case response := <-resultChannel:
		return decodeResponse(response)
	case <-pending.ctx.Done():
		select {
		case response := <-resultChannel:
			return decodeResponse(response)
		default:
			return pending.ctx.Err()
		}
	case <-callContext.Done():
		return callContext.Err()
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
}

func (b *Bridge) cancelPendingLocked(id string, pending *pendingCall) {
	pending.cancel()
	if !pending.dispatched {
		delete(b.pending, id)
		return
	}
	instance := b.instances[pending.browserID]
	if !pending.cancelSent && instance != nil && instance.peer.BootID == pending.bootID {
		select {
		case instance.controls <- command{ID: id, BootID: pending.bootID, Method: "cancel"}:
			pending.cancelSent = true
		default:
		}
	}
}

func (b *Bridge) receiveResult(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !b.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	var result response
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<20)).Decode(&result); err != nil || result.ID == "" || result.InstanceID == "" || result.BootID == "" {
		http.Error(writer, "invalid result", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	instance := b.instances[result.InstanceID]
	pending := b.pending[result.ID]
	if instance == nil || result.BootID != instance.peer.BootID || pending != nil && (pending.browserID != result.InstanceID || pending.bootID != result.BootID) {
		b.mu.Unlock()
		http.Error(writer, "result does not belong to this instance generation", http.StatusConflict)
		return
	}
	if pending == nil {
		b.mu.Unlock()
		http.Error(writer, "unknown command", http.StatusNotFound)
		return
	}
	if result.BootID == instance.peer.BootID && instance.socket != nil {
		instance.lastSeen = time.Now()
	}
	delete(b.pending, result.ID)
	if pending.stopGateWatch != nil {
		pending.stopGateWatch()
	}
	if pending.release != nil {
		pending.release()
	}
	if !pending.abandoned {
		pending.result <- result
	}
	pending.cancel()
	b.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (b *Bridge) authorizeDesktop(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", 405)
		return
	}
	if !b.authorized(request) {
		http.Error(writer, "unauthorized", 401)
		return
	}
	var identity response
	if json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096)).Decode(&identity) != nil {
		http.Error(writer, "invalid command identity", 400)
		return
	}
	b.mu.Lock()
	pending := b.pending[identity.ID]
	gate := b.gate
	if pending == nil || !pending.dispatched || pending.abandoned || pending.browserID != identity.InstanceID || pending.bootID != identity.BootID {
		b.mu.Unlock()
		http.Error(writer, "command canceled or generation changed", 409)
		return
	}
	if pending.release != nil {
		b.mu.Unlock()
		writer.WriteHeader(204)
		return
	}
	if pending.authorizing {
		b.mu.Unlock()
		http.Error(writer, "desktop authorization is already in progress", http.StatusConflict)
		return
	}
	pending.authorizing = true
	operationContext, controlID := pending.ctx, pending.controlID
	b.mu.Unlock()
	if gate == nil {
		b.mu.Lock()
		pending.authorizing = false
		abandoned := pending.abandoned
		if abandoned && b.pending[identity.ID] == pending {
			delete(b.pending, identity.ID)
		}
		b.mu.Unlock()
		if abandoned {
			http.Error(writer, "command canceled", http.StatusConflict)
			return
		}
		writer.WriteHeader(204)
		return
	}
	controlledContext, release, err := gate(operationContext, controlID)
	if err != nil {
		b.mu.Lock()
		pending.authorizing = false
		if pending.abandoned && b.pending[identity.ID] == pending && pending.release == nil {
			delete(b.pending, identity.ID)
		}
		b.mu.Unlock()
		http.Error(writer, err.Error(), 409)
		return
	}
	b.mu.Lock()
	if b.pending[identity.ID] != pending || pending.abandoned {
		if b.pending[identity.ID] == pending {
			delete(b.pending, identity.ID)
		}
		b.mu.Unlock()
		if release != nil {
			release()
		}
		http.Error(writer, "command canceled", 409)
		return
	}
	if controlledContext != nil && controlledContext.Err() != nil {
		pending.authorizing = false
		if pending.abandoned && b.pending[identity.ID] == pending {
			delete(b.pending, identity.ID)
		}
		b.mu.Unlock()
		if release != nil {
			release()
		}
		http.Error(writer, "desktop control was canceled", http.StatusConflict)
		return
	}
	pending.release = release
	pending.authorizing = false
	if controlledContext == nil {
		controlledContext = operationContext
	}
	pending.stopGateWatch = context.AfterFunc(controlledContext, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.pending[identity.ID] == pending {
			b.cancelPendingLocked(identity.ID, pending)
		}
	})
	b.mu.Unlock()
	writer.WriteHeader(204)
}

func (b *Bridge) authorized(request *http.Request) bool {
	provided, bearer := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return bearer && len(provided) == len(b.token) && subtle.ConstantTimeCompare([]byte(provided), []byte(b.token)) == 1
}

func validateAction(action Action) error {
	if action.TabID == "" {
		return errors.New("tab_id is required")
	}
	targetCount := func() int {
		count := 0
		for _, present := range []bool{action.Ref != "", action.ScreenshotID != ""} {
			if present {
				count++
			}
		}
		return count
	}
	requireScreenshotPoint := func() error {
		if action.ScreenshotID != "" && (action.X == nil || action.Y == nil || *action.X < 0 || *action.Y < 0) {
			return errors.New("screenshot targeting requires non-negative x and y")
		}
		return nil
	}
	if action.Button != "" && action.Button != "left" && action.Button != "middle" && action.Button != "right" {
		return errors.New("button must be left, middle, or right")
	}
	switch action.Kind {
	case "click", "double_click", "hover", "drag":
		if targetCount() != 1 {
			return fmt.Errorf("%s requires ref or screenshot_id coordinates", action.Kind)
		}
		if err := requireScreenshotPoint(); err != nil {
			return err
		}
		if action.Kind == "drag" {
			if action.ToX == nil || action.ToY == nil || *action.ToX < 0 || *action.ToY < 0 {
				return errors.New("drag requires non-negative to_x and to_y")
			}
		}
	case "type_text", "set_value":
		if targetCount() != 1 || action.ScreenshotID != "" {
			return fmt.Errorf("%s requires ref", action.Kind)
		}
		if action.Text == "" {
			return fmt.Errorf("%s requires text", action.Kind)
		}
	case "press_key":
		if action.Key == "" {
			return errors.New("press_key requires key")
		}
		if targetCount() > 1 || action.ScreenshotID != "" {
			return errors.New("press_key accepts at most one ref")
		}
	case "scroll":
		if action.ScrollX == 0 && action.ScrollY == 0 {
			return errors.New("scroll requires scroll_x or scroll_y")
		}
		if targetCount() > 1 {
			return errors.New("scroll accepts at most one target")
		}
		if err := requireScreenshotPoint(); err != nil {
			return err
		}
	case "select":
		if targetCount() != 1 || action.ScreenshotID != "" || action.Option == "" {
			return errors.New("select requires ref and option")
		}
	case "check":
		if targetCount() != 1 || action.ScreenshotID != "" || action.Checked == nil {
			return errors.New("check requires ref and checked")
		}
	case "upload_files":
		if targetCount() != 1 || action.ScreenshotID != "" || len(action.Files) == 0 {
			return errors.New("upload_files requires ref and files")
		}
	case "handle_dialog":
		if action.Accept == nil {
			return errors.New("handle_dialog requires accept")
		}
	case "evaluate":
		if action.Script == "" {
			return errors.New("evaluate requires script")
		}
	default:
		return fmt.Errorf("unsupported browser action %q", action.Kind)
	}
	return nil
}

//go:embed extension/*
var extensionFiles embed.FS

func InstallExtension(directory string) (string, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	entries, err := extensionFiles.ReadDir("extension")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := extensionFiles.ReadFile("extension/" + entry.Name())
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(abs, entry.Name()), data, 0o644); err != nil {
			return "", err
		}
	}
	return abs, nil
}

func ConfigureExtension(directory, address, token string) error {
	if address == "" || len(token) < 32 {
		return errors.New("extension configuration requires a bridge address and a token of at least 32 characters")
	}
	if !strings.HasPrefix(address, "127.0.0.1:") && !strings.HasPrefix(address, "localhost:") && !strings.HasPrefix(address, "[::1]:") {
		return errors.New("extension bridge address must use a loopback host")
	}
	payload := fmt.Sprintf("export const packagedConfig = { address: %q, token: %q };\n", address, token)
	path := filepath.Join(directory, "runtime_config.js")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func decodeBase64(value string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode browser screenshot: %w", err)
	}
	return data, nil
}
