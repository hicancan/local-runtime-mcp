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
	"io/fs"
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
	"github.com/hicancan/local-runtime-mcp/internal/version"
	"golang.org/x/net/websocket"
)

const ExtensionVersion = version.Version

// DesktopGate reserves the shared interactive desktop for a visible browser operation.
type DesktopGate func(context.Context, string) (context.Context, func(), error)

// Bridge unifies:
//  1. Direct native browser-level CDP connections (via DevToolsActivePort or explicit
//     cdp_endpoints) — the single source of truth for ALL browser automation, full
//     Chrome DevTools, and browser-process-level Tracing.start.
//  2. The thin loopback extension server (127.0.0.1:9315) strictly for Web-to-Local
//     capabilities (/v1/web), owner grants panel (/v1/grants), and optional Edge
//     visual Tab Group styling — zero chrome.debugger overlap.
type Bridge struct {
	configured     bool
	disableAutoCDP bool
	cdpEndpoints   []string
	unavailable    string
	address        string
	token          string
	server         *http.Server

	mu           sync.Mutex
	cdpConns     map[string]*cdpEntry // browserID -> managed CDP connection
	lastDiscover time.Time
	tabOwners    map[string]*tabOwnership
	instances  map[string]*instance // thin extension peers (for web_bridge + tab groups)
	extTabs    map[string]tabRoute
	gate       DesktopGate
	webHandler WebHandler
	grants     GrantHandler
	webActive  int

	ctx       context.Context
	cancel    context.CancelFunc
	sequence  atomic.Uint64
	prefix    string
	closeOnce sync.Once
	closeErr  error
	sockets   map[*websocket.Conn]struct{}
	socketWG  sync.WaitGroup
}

type cdpEntry struct {
	discovered DiscoveredBrowser
	state      string // "offline", "awaiting_approval", "connected", "rejected"
	lastError  string
	lastSeen   time.Time
	client     *cdpClient
	dialing    bool
	rejectedFP string
}

type tabOwnership struct {
	browserID string
	targetID  string
	sessionID string
	groupName string
	handedOff bool
}

type instance struct {
	peer     Peer
	lastSeen time.Time
	commands chan command
	controls chan command
	socket   *websocket.Conn
}

type tabRoute struct{ browserID, bootID string }

type Peer struct {
	InstanceID       string `json:"instance_id,omitempty"`
	BootID           string `json:"boot_id"`
	Label            string `json:"label"`
	Browser          string `json:"browser,omitempty"`
	ExtensionVersion string `json:"extension_version,omitempty"`
}

type Status struct {
	Configured         bool             `json:"configured"`
	Unavailable        string           `json:"unavailable,omitempty"`
	Connected          bool             `json:"connected"`
	Address            string           `json:"address,omitempty"`
	ExtensionConnected bool             `json:"extension_connected"`
	Instances          []InstanceStatus `json:"instances"`
}

type InstanceStatus struct {
	BrowserID        string    `json:"browser_id"`
	Label            string    `json:"label"`
	Browser          string    `json:"browser"`
	State            string    `json:"state"` // "connected", "awaiting_approval", "rejected", "offline"
	CDPURL           string    `json:"cdp_url,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	ExtensionVersion string    `json:"extension_version,omitempty"`
	Connected        bool      `json:"connected"`
	LastSeen         time.Time `json:"last_seen"`
}

type Tab struct {
	ID        string `json:"id"`
	TargetID  string `json:"target_id,omitempty"`
	BrowserID string `json:"browser_id"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Active    bool   `json:"active"`
	HandedOff bool   `json:"handed_off,omitempty"`
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
	Diff              bool              `json:"diff,omitempty"`
	AddedLines        int               `json:"added_lines,omitempty"`
	RemovedLines      int               `json:"removed_lines,omitempty"`
	Elements          []SnapshotElement `json:"elements"`
	ElementsTruncated bool              `json:"elements_truncated"`
}

type ScreenshotOptions struct {
	TabID    string `json:"tab_id"`
	FullPage bool   `json:"full_page,omitempty"`
	ClipX    int    `json:"clip_x,omitempty"`
	ClipY    int    `json:"clip_y,omitempty"`
	ClipW    int    `json:"clip_width,omitempty"`
	ClipH    int    `json:"clip_height,omitempty"`
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
	Kind         string   `json:"kind"`
	TabID        string   `json:"tab_id"`
	ControlID    string   `json:"control_id,omitempty"`
	Ref          string   `json:"ref,omitempty"`
	Locator      Locator  `json:"locator,omitempty"`
	ScreenshotID string   `json:"screenshot_id,omitempty"`
	X            *float64 `json:"x,omitempty"`
	Y            *float64 `json:"y,omitempty"`
	ToX          *float64 `json:"to_x,omitempty"`
	ToY          *float64 `json:"to_y,omitempty"`
	Button       string   `json:"button,omitempty"`
	Text         string   `json:"text,omitempty"`
	Key          string   `json:"key,omitempty"`
	ScrollX      int      `json:"scroll_x,omitempty"`
	ScrollY      int      `json:"scroll_y,omitempty"`
	Option       string   `json:"option,omitempty"`
	Checked      *bool    `json:"checked,omitempty"`
	Files        []string `json:"files,omitempty"`
	Accept       *bool    `json:"accept,omitempty"`
	PromptText   string   `json:"prompt_text,omitempty"`
	Script       string   `json:"script,omitempty"`
}

type Navigation struct {
	TabID     string `json:"tab_id"`
	ControlID string `json:"control_id,omitempty"`
	Kind      string `json:"kind"` // url, back, forward, reload
	URL       string `json:"url,omitempty"`
}

type ActionResult struct {
	TabID   string `json:"tab_id"`
	Kind    string `json:"kind"`
	Success bool   `json:"success"`
	Value   any    `json:"value,omitempty"`
}

type OpenOptions struct {
	BrowserID   string
	URL         string
	Active      bool
	ControlID   string
	SessionID   string
	SessionName string
}

type command struct {
	ID       string          `json:"id"`
	BootID   string          `json:"boot_id"`
	Deadline int64           `json:"deadline"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params,omitempty"`
}

func Start(ctx context.Context, configuration config.Browser) (*Bridge, error) {
	bridgeContext, cancel := context.WithCancel(ctx)
	bridge := &Bridge{
		configured:     true,
		disableAutoCDP: configuration.DisableAutoCDP,
		cdpEndpoints:   append([]string(nil), configuration.CDPEndpoints...),
		token:          configuration.Token,
		cdpConns:       map[string]*cdpEntry{},
		tabOwners:      map[string]*tabOwnership{},
		instances:      map[string]*instance{},
		extTabs:        map[string]tabRoute{},
		sockets:        map[*websocket.Conn]struct{}{},
		ctx:            bridgeContext,
		cancel:         cancel,
		prefix:         rand.Text(),
	}

	// Discover local Chromium DevToolsActivePort files & configured CDP endpoints immediately.
	bridge.refreshDiscovered(true)

	// Optionally start the thin extension bridge (127.0.0.1:9315) for Web Bridge,
	// owner Grants panel, and visual Tab Groups (never chrome.debugger).
	if configuration.Token != "" {
		listen := configuration.Listen
		if listen == "" {
			listen = config.DefaultBrowser
		}
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			bridge.unavailable = fmt.Sprintf("extension bridge %s unavailable: %v", listen, err)
		} else {
			bridge.address = listener.Addr().String()
			mux := http.NewServeMux()
			mux.Handle("/v1/connect", websocket.Server{Handshake: bridge.socketHandshake, Handler: bridge.connect})
			mux.HandleFunc("/v1/web", bridge.receiveWeb)
			mux.HandleFunc("/v1/grants", bridge.receiveGrants)
			bridge.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			go func() { _ = bridge.server.Serve(listener) }()
		}
	}

	// Eagerly connect explicit configured endpoints (e.g., --remote-debugging-port or tests).
	for _, raw := range bridge.cdpEndpoints {
		if strings.TrimSpace(raw) != "" {
			go func() {
				_, _ = bridge.Connect(bridgeContext, "")
			}()
			break
		}
	}

	go func() {
		<-bridgeContext.Done()
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = bridge.Close(shutdownContext)
	}()

	return bridge, nil
}

func (b *Bridge) Close(ctx context.Context) error {
	b.closeOnce.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		b.mu.Lock()
		clients := make([]*cdpClient, 0, len(b.cdpConns))
		for _, entry := range b.cdpConns {
			if entry.client != nil {
				clients = append(clients, entry.client)
				entry.client = nil
				entry.state = "offline"
			}
		}
		sockets := make([]*websocket.Conn, 0, len(b.sockets))
		for socket := range b.sockets {
			sockets = append(sockets, socket)
		}
		b.mu.Unlock()

		for _, cl := range clients {
			cl.Close()
		}
		for _, socket := range sockets {
			_ = socket.SetDeadline(time.Now())
			_ = socket.Close()
		}
		if b.server != nil {
			shutdownContext, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			b.closeErr = b.server.Shutdown(shutdownContext)
			if errors.Is(b.closeErr, context.DeadlineExceeded) {
				b.closeErr = b.server.Close()
			}
		}
		b.socketWG.Wait()
	})
	return b.closeErr
}

func (b *Bridge) SetDesktopGate(gate DesktopGate) {
	b.mu.Lock()
	b.gate = gate
	b.mu.Unlock()
}

const discoverTTL = 2 * time.Second

func (b *Bridge) refreshDiscovered(force bool) {
	if !force {
		b.mu.Lock()
		fresh := !b.lastDiscover.IsZero() && time.Since(b.lastDiscover) < discoverTTL
		b.mu.Unlock()
		if fresh {
			return
		}
	}
	found := DiscoverBrowsers(b.cdpEndpoints, b.disableAutoCDP)
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastDiscover = now
	active := map[string]bool{}
	for _, d := range found {
		active[d.ID] = true
		existing, ok := b.cdpConns[d.ID]
		if !ok {
			b.cdpConns[d.ID] = &cdpEntry{
				discovered: d,
				state:      "offline",
			}
			continue
		}
		if existing.discovered.Fingerprint != d.Fingerprint {
			// Browser restarted with a new DevToolsActivePort.
			if existing.client != nil {
				old := existing.client
				existing.client = nil
				go old.Close()
			}
			existing.discovered = d
			existing.state = "offline"
			existing.rejectedFP = ""
			existing.lastError = ""
		} else if existing.client != nil && !existing.client.Alive() {
			existing.client = nil
			existing.state = "offline"
		}
	}
	for id, entry := range b.cdpConns {
		if !active[id] && (entry.client == nil || !entry.client.Alive()) {
			delete(b.cdpConns, id)
		}
	}
}

// Status reports all discovered/connected Chromium CDP instances plus extension panel link state.
func (b *Bridge) Status() Status {
	b.refreshDiscovered(true)
	b.mu.Lock()
	defer b.mu.Unlock()

	extConnected := false
	extVer := ""
	for _, inst := range b.instances {
		if inst.socket != nil && time.Since(inst.lastSeen) < 35*time.Second {
			extConnected = true
			extVer = inst.peer.ExtensionVersion
			break
		}
	}

	status := Status{
		Configured:         b.configured,
		Unavailable:        b.unavailable,
		Address:            b.address,
		ExtensionConnected: extConnected,
		Instances:          make([]InstanceStatus, 0, len(b.cdpConns)),
	}
	for id, entry := range b.cdpConns {
		connected := entry.client != nil && entry.client.Alive()
		st := entry.state
		if connected {
			st = "connected"
			status.Connected = true
		}
		browserStr := entry.discovered.Label
		if entry.client != nil && entry.client.ProductVersion() != "" {
			browserStr = entry.client.ProductVersion()
		}
		status.Instances = append(status.Instances, InstanceStatus{
			BrowserID:        id,
			Label:            entry.discovered.Label,
			Browser:          browserStr,
			State:            st,
			CDPURL:           entry.discovered.WSURL,
			LastError:        entry.lastError,
			ExtensionVersion: extVer,
			Connected:        connected,
			LastSeen:         entry.lastSeen,
		})
	}
	sort.Slice(status.Instances, func(i, j int) bool { return status.Instances[i].BrowserID < status.Instances[j].BrowserID })
	return status
}

// Connect establishes (or returns) a persistent browser-level CDP WebSocket for browserID.
// When browserID is empty, it selects the first available connected or discovered browser.
func (b *Bridge) Connect(ctx context.Context, browserID string) (InstanceStatus, error) {
	cl, err := b.resolveClient(ctx, browserID, true)
	if err != nil {
		return InstanceStatus{}, err
	}
	for _, inst := range b.Status().Instances {
		if inst.BrowserID == cl.id {
			return inst, nil
		}
	}
	return InstanceStatus{BrowserID: cl.id, Label: cl.label, Browser: cl.ProductVersion(), State: "connected", Connected: true, LastSeen: time.Now()}, nil
}

func (b *Bridge) selectEntryLocked(browserID string) *cdpEntry {
	if browserID != "" {
		return b.cdpConns[browserID]
	}
	// Prefer already-connected browser, then edge, then chrome, then alphabetical.
	var bestID string
	var bestEntry *cdpEntry
	bestRank := 99
	for id, e := range b.cdpConns {
		r := 3
		if e.client != nil && e.client.Alive() {
			r = 0
		} else if id == "edge" {
			r = 1
		} else if id == "chrome" {
			r = 2
		}
		if bestEntry == nil || r < bestRank || (r == bestRank && id < bestID) {
			bestID = id
			bestEntry = e
			bestRank = r
		}
	}
	return bestEntry
}

func (b *Bridge) resolveClient(ctx context.Context, browserID string, forceRetry bool) (*cdpClient, error) {
	// Zero-I/O fast path: reuse an already-connected live CDP client without
	// scanning DevToolsActivePort files on every browser operation.
	if !forceRetry {
		b.mu.Lock()
		entry := b.selectEntryLocked(browserID)
		if entry != nil && entry.client != nil && entry.client.Alive() {
			cl := entry.client
			entry.lastSeen = time.Now()
			b.mu.Unlock()
			return cl, nil
		}
		b.mu.Unlock()
	}

	b.refreshDiscovered(forceRetry)

	b.mu.Lock()
	entry := b.selectEntryLocked(browserID)
	if entry == nil {
		b.mu.Unlock()
		if browserID != "" {
			return nil, fmt.Errorf("browser %q not found; ensure remote debugging is enabled (edge://inspect or chrome://inspect/#remote-debugging)", browserID)
		}
		return nil, errors.New("no Chromium browser with DevToolsActivePort found; enable remote debugging in edge://inspect or chrome://inspect/#remote-debugging")
	}
	if entry.client != nil && entry.client.Alive() {
		cl := entry.client
		entry.lastSeen = time.Now()
		b.mu.Unlock()
		return cl, nil
	}
	if !forceRetry && entry.rejectedFP == entry.discovered.Fingerprint {
		b.mu.Unlock()
		return nil, fmt.Errorf("remote debugging connection to %s was declined; call browser.connect(%q) to retry", entry.discovered.Label, entry.discovered.ID)
	}
	if !forceRetry && entry.state == "awaiting_approval" {
		b.mu.Unlock()
		return nil, fmt.Errorf("%s is awaiting one-time 'Allow' on its remote debugging prompt (%s); click Allow in the browser then call browser.connect(%q)", entry.discovered.Label, entry.discovered.WSURL, entry.discovered.ID)
	}
	if entry.dialing {
		b.mu.Unlock()
		return nil, fmt.Errorf("%s already has a pending remote debugging connection awaiting one-time 'Allow' (%s)", entry.discovered.Label, entry.discovered.WSURL)
	}
	entry.state = "awaiting_approval"
	entry.dialing = true
	disc := entry.discovered
	b.mu.Unlock()

	wsURL := disc.WSURL
	if wsURL == "" && disc.HTTPURL != "" {
		resolved, _, err := resolveHTTPCDPURL(ctx, disc.HTTPURL)
		if err != nil {
			b.mu.Lock()
			entry.dialing = false
			entry.state = "offline"
			entry.lastError = err.Error()
			b.mu.Unlock()
			return nil, err
		}
		wsURL = resolved
	}

	dialCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
	}

	cl, err := dialCDP(dialCtx, disc.ID, disc.Label, wsURL, disc.Fingerprint)
	b.mu.Lock()
	entry.dialing = false
	if err != nil {
		entry.lastError = err.Error()
		if errors.Is(err, context.Canceled) {
			entry.state = "offline"
			b.mu.Unlock()
			return nil, err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			entry.state = "awaiting_approval"
			b.mu.Unlock()
			return nil, fmt.Errorf("%s is waiting for one-time 'Allow' on its remote debugging prompt (%s)", disc.Label, wsURL)
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			entry.state = "offline"
			b.mu.Unlock()
			return nil, fmt.Errorf("connect CDP %s: %w", disc.Label, err)
		}
		entry.state = "rejected"
		entry.rejectedFP = disc.Fingerprint
		b.mu.Unlock()
		return nil, fmt.Errorf("connect CDP %s: %w", disc.Label, err)
	}
	entry.client = cl
	entry.state = "connected"
	entry.lastError = ""
	entry.rejectedFP = ""
	entry.lastSeen = time.Now()
	b.mu.Unlock()
	return cl, nil
}

func makeTabHandle(browserID, targetID string) string {
	return browserID + ":" + targetID
}

func parseTabHandle(tabID string) (browserID, targetID string, err error) {
	idx := strings.IndexByte(tabID, ':')
	if idx <= 0 || idx >= len(tabID)-1 {
		return "", "", fmt.Errorf("invalid tab_id %q; expected <browser_id>:<target_id>", tabID)
	}
	return tabID[:idx], tabID[idx+1:], nil
}

func (b *Bridge) resolveTab(ctx context.Context, tabID string) (*cdpClient, string, error) {
	browserID, targetID, err := parseTabHandle(tabID)
	if err != nil {
		return nil, "", err
	}
	cl, err := b.resolveClient(ctx, browserID, false)
	if err != nil {
		return nil, "", err
	}
	return cl, targetID, nil
}

func (b *Bridge) acquireDesktopIfNeeded(ctx context.Context, controlID string, visible bool) (context.Context, func(), error) {
	if !visible {
		return ctx, func() {}, nil
	}
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if gate == nil {
		return ctx, func() {}, nil
	}
	controlled, release, err := gate(ctx, controlID)
	if err != nil {
		return nil, nil, err
	}
	if controlled == nil {
		controlled = ctx
	}
	if release == nil {
		release = func() {}
	}
	return controlled, release, nil
}

// Tabs lists controllable tabs (and optionally service workers/all targets) in browserID.
func (b *Bridge) Tabs(ctx context.Context, browserID string) ([]Tab, error) {
	return b.TabsWithOptions(ctx, browserID, false)
}

func (b *Bridge) TabsWithOptions(ctx context.Context, browserID string, includeWorkers bool) ([]Tab, error) {
	cl, err := b.resolveClient(ctx, browserID, false)
	if err != nil {
		return nil, err
	}
	var res struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			Title    string `json:"title"`
			URL      string `json:"url"`
			Attached bool   `json:"attached"`
		} `json:"targetInfos"`
	}
	if err := cl.Send(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tabs := make([]Tab, 0, len(res.TargetInfos))
	for _, info := range res.TargetInfos {
		switch info.Type {
		case "page":
			if strings.HasPrefix(info.URL, "devtools://") || strings.HasPrefix(info.URL, "chrome-extension://") {
				continue
			}
		case "service_worker", "worker", "shared_worker":
			if !includeWorkers {
				continue
			}
		default:
			continue
		}
		handle := makeTabHandle(cl.id, info.TargetID)
		handed := false
		if own := b.tabOwners[handle]; own != nil {
			handed = own.handedOff
		}
		tabs = append(tabs, Tab{
			ID:        handle,
			TargetID:  info.TargetID,
			BrowserID: cl.id,
			Type:      info.Type,
			Title:     info.Title,
			URL:       info.URL,
			HandedOff: handed,
		})
	}
	return tabs, nil
}

// Open creates a new tab concurrently via Target.createTarget (zero single-lane serialization).
func (b *Bridge) Open(ctx context.Context, browserID, targetURL string, active bool, controlID string) (Tab, error) {
	return b.OpenWithOptions(ctx, OpenOptions{
		BrowserID: browserID,
		URL:       targetURL,
		Active:    active,
		ControlID: controlID,
	})
}

func (b *Bridge) OpenWithOptions(ctx context.Context, opts OpenOptions) (Tab, error) {
	if opts.URL == "" {
		opts.URL = "about:blank"
	}
	opCtx, release, err := b.acquireDesktopIfNeeded(ctx, opts.ControlID, opts.Active)
	if err != nil {
		return Tab{}, err
	}
	defer release()

	cl, err := b.resolveClient(opCtx, opts.BrowserID, false)
	if err != nil {
		return Tab{}, err
	}
	// Create at about:blank first, attach & enable Network/Page/Runtime/DOM,
	// then navigate via Page.navigate so main-document requests are captured
	// from byte 0 and waitReady never mistakes initial about:blank for loaded.
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := cl.Send(opCtx, "", "Target.createTarget", map[string]any{
		"url":        "about:blank",
		"background": !opts.Active,
	}, &created); err != nil {
		return Tab{}, err
	}
	if created.TargetID == "" {
		return Tab{}, errors.New("browser did not return targetId")
	}

	ts, err := cl.ensureAttached(opCtx, created.TargetID)
	if err != nil {
		return Tab{}, err
	}
	if opts.URL != "about:blank" {
		ts.mu.Lock()
		prevNav := ts.navSeq
		ts.mu.Unlock()
		loadCh, cancelLoad := ts.subscribeLoad()
		defer cancelLoad()

		var navRes struct {
			ErrorText string `json:"errorText"`
		}
		if err := cl.Send(opCtx, ts.sessionID, "Page.navigate", map[string]any{"url": opts.URL}, &navRes); err != nil {
			return Tab{}, err
		}
		if navRes.ErrorText != "" {
			return Tab{}, fmt.Errorf("navigate %s: %s", opts.URL, navRes.ErrorText)
		}
		_ = cl.waitNavigation(opCtx, ts, loadCh, prevNav, true, 20*time.Second)
	}
	title, finalURL := cl.targetMeta(opCtx, created.TargetID)
	if finalURL == "" {
		finalURL = opts.URL
	}
	handle := makeTabHandle(cl.id, created.TargetID)
	if opts.SessionID != "" || opts.SessionName != "" {
		b.mu.Lock()
		b.tabOwners[handle] = &tabOwnership{
			browserID: cl.id,
			targetID:  created.TargetID,
			sessionID: opts.SessionID,
			groupName: opts.SessionName,
		}
		b.mu.Unlock()
	}
	if opts.SessionName != "" {
		go b.notifyTabGroup(finalURL, opts.SessionName)
	}
	return Tab{
		ID:        handle,
		TargetID:  created.TargetID,
		BrowserID: cl.id,
		Type:      "page",
		Title:     title,
		URL:       finalURL,
		Active:    opts.Active,
	}, nil
}

func (b *Bridge) notifyTabGroup(tabURL, groupName string) {
	payload, err := json.Marshal(map[string]any{"url": tabURL, "title": groupName})
	if err != nil {
		return
	}
	b.mu.Lock()
	for _, inst := range b.instances {
		if inst.socket != nil && time.Since(inst.lastSeen) < 35*time.Second {
			select {
			case inst.commands <- command{
				ID:       fmt.Sprintf("%s-%d", b.prefix, b.sequence.Add(1)),
				BootID:   inst.peer.BootID,
				Deadline: time.Now().Add(5 * time.Second).UnixMilli(),
				Method:   "tabs.group",
				Params:   payload,
			}:
			default:
			}
		}
	}
	b.mu.Unlock()
}

func (c *cdpClient) waitNavigation(ctx context.Context, ts *tabSession, loadCh <-chan struct{}, prevNav int, requireLeaveBlank bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-loadCh:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil
			}
			ts.mu.Lock()
			navAdvanced := ts.navSeq > prevNav
			ts.mu.Unlock()
			var res struct {
				Result struct {
					Value map[string]any `json:"value"`
				} `json:"result"`
			}
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := c.Send(checkCtx, ts.sessionID, "Runtime.evaluate", map[string]any{
				"expression":    `({ ready: document.readyState, href: location.href })`,
				"returnByValue": true,
			}, &res)
			cancel()
			if err != nil || res.Result.Value == nil {
				continue
			}
			ready, _ := res.Result.Value["ready"].(string)
			href, _ := res.Result.Value["href"].(string)
			if requireLeaveBlank && (href == "" || href == "about:blank") {
				continue
			}
			if ready == "complete" && (navAdvanced || requireLeaveBlank) {
				return nil
			}
		}
	}
}

func (c *cdpClient) waitReady(ctx context.Context, ts *tabSession, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var res struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.Send(checkCtx, ts.sessionID, "Runtime.evaluate", map[string]any{
			"expression":    "document.readyState",
			"returnByValue": true,
		}, &res)
		cancel()
		if err == nil && res.Result.Value == "complete" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(120 * time.Millisecond):
		}
	}
	return nil
}

func (b *Bridge) CloseTab(ctx context.Context, tabID, controlID string) error {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return err
	}
	if err := cl.Send(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.tabOwners, tabID)
	b.mu.Unlock()
	return nil
}

func (b *Bridge) MarkHandoff(tabID string) error {
	browserID, targetID, err := parseTabHandle(tabID)
	if err != nil {
		return err
	}
	b.mu.Lock()
	own := b.tabOwners[tabID]
	if own == nil {
		own = &tabOwnership{browserID: browserID, targetID: targetID}
		b.tabOwners[tabID] = own
	}
	own.handedOff = true
	var cl *cdpClient
	if entry := b.cdpConns[browserID]; entry != nil && entry.client != nil && entry.client.Alive() {
		cl = entry.client
	}
	b.mu.Unlock()
	if cl != nil {
		cl.releaseBgCapturerByTarget(targetID)
	}
	return nil
}

// CleanupSessionTabs closes all temporary tabs opened by sessionID unless explicitly marked for handoff.
func (b *Bridge) CleanupSessionTabs(ctx context.Context, sessionID string) int {
	if sessionID == "" {
		return 0
	}
	b.mu.Lock()
	var toClose []string
	for handle, own := range b.tabOwners {
		if own.sessionID == sessionID && !own.handedOff {
			toClose = append(toClose, handle)
		}
	}
	b.mu.Unlock()

	closed := 0
	for _, handle := range toClose {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if b.CloseTab(closeCtx, handle, "") == nil {
			closed++
		}
		cancel()
	}
	return closed
}

func (b *Bridge) Navigate(ctx context.Context, nav Navigation) (Tab, error) {
	cl, targetID, err := b.resolveTab(ctx, nav.TabID)
	if err != nil {
		return Tab{}, err
	}
	ts, err := cl.ensureAttached(ctx, targetID)
	if err != nil {
		return Tab{}, err
	}
	ts.mu.Lock()
	prevNav := ts.navSeq
	ts.mu.Unlock()
	loadCh, cancelLoad := ts.subscribeLoad()
	defer cancelLoad()

	switch nav.Kind {
	case "url":
		if strings.TrimSpace(nav.URL) == "" {
			return Tab{}, errors.New("url navigation requires url")
		}
		var navRes struct {
			ErrorText string `json:"errorText"`
		}
		if err := cl.Send(ctx, ts.sessionID, "Page.navigate", map[string]any{"url": nav.URL}, &navRes); err != nil {
			return Tab{}, err
		}
		if navRes.ErrorText != "" {
			return Tab{}, fmt.Errorf("navigate %s: %s", nav.URL, navRes.ErrorText)
		}
	case "reload":
		if err := cl.Send(ctx, ts.sessionID, "Page.reload", nil, nil); err != nil {
			return Tab{}, err
		}
	case "back", "forward":
		var hist struct {
			CurrentIndex int `json:"currentIndex"`
			Entries      []struct {
				ID int `json:"id"`
			} `json:"entries"`
		}
		if err := cl.Send(ctx, ts.sessionID, "Page.getNavigationHistory", nil, &hist); err != nil {
			return Tab{}, err
		}
		next := hist.CurrentIndex - 1
		if nav.Kind == "forward" {
			next = hist.CurrentIndex + 1
		}
		if next < 0 || next >= len(hist.Entries) {
			return Tab{}, fmt.Errorf("no %s history entry available", nav.Kind)
		}
		if err := cl.Send(ctx, ts.sessionID, "Page.navigateToHistoryEntry", map[string]any{"entryId": hist.Entries[next].ID}, nil); err != nil {
			return Tab{}, err
		}
	default:
		return Tab{}, fmt.Errorf("unsupported browser navigation %q", nav.Kind)
	}
	_ = cl.waitNavigation(ctx, ts, loadCh, prevNav, false, 20*time.Second)
	title, finalURL := cl.targetMeta(ctx, targetID)
	return Tab{ID: nav.TabID, TargetID: targetID, BrowserID: cl.id, Type: "page", Title: title, URL: finalURL}, nil
}

func (b *Bridge) Snapshot(ctx context.Context, tabID string, maxElements, maxText int) (Snapshot, error) {
	return b.SnapshotWithOptions(ctx, tabID, SnapshotOptions{MaxElements: maxElements, MaxText: maxText, Diff: false})
}

func (b *Bridge) SnapshotWithOptions(ctx context.Context, tabID string, opts SnapshotOptions) (Snapshot, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return Snapshot{}, err
	}
	return cl.snapshotTab(ctx, tabID, targetID, opts)
}

func (b *Bridge) Screenshot(ctx context.Context, opts ScreenshotOptions) ([]byte, ScreenshotInfo, error) {
	cl, targetID, err := b.resolveTab(ctx, opts.TabID)
	if err != nil {
		return nil, ScreenshotInfo{}, err
	}
	return cl.screenshotTab(ctx, opts.TabID, targetID, opts)
}

func (b *Bridge) Act(ctx context.Context, action Action) (ActionResult, error) {
	cl, targetID, err := b.resolveTab(ctx, action.TabID)
	if err != nil {
		return ActionResult{}, err
	}
	return cl.actTab(ctx, action.TabID, targetID, action)
}

func (b *Bridge) Network(ctx context.Context, tabID string, filter NetworkFilter) ([]NetworkEntry, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return nil, err
	}
	return cl.networkTab(ctx, targetID, filter)
}

func (b *Bridge) NetworkBody(ctx context.Context, tabID, requestID string) (NetworkBodyResult, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return NetworkBodyResult{}, err
	}
	return cl.networkBodyTab(ctx, targetID, requestID)
}

func (b *Bridge) Console(ctx context.Context, tabID string, filter ConsoleFilter) ([]ConsoleEntry, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return nil, err
	}
	return cl.consoleTab(ctx, targetID, filter)
}

func (b *Bridge) TraceStart(ctx context.Context, opts TraceStartOptions) error {
	var cl *cdpClient
	var reloadTargetID string
	var err error
	if opts.TabID != "" {
		cl, reloadTargetID, err = b.resolveTab(ctx, opts.TabID)
	} else {
		cl, err = b.resolveClient(ctx, opts.BrowserID, false)
	}
	if err != nil {
		return err
	}
	return cl.traceStart(ctx, opts, reloadTargetID)
}

func (b *Bridge) TraceStop(ctx context.Context, browserID, filePath string) (TraceSummary, error) {
	cl, err := b.resolveClient(ctx, browserID, false)
	if err != nil {
		return TraceSummary{}, err
	}
	return cl.traceStop(ctx, filePath)
}

func (b *Bridge) Emulate(ctx context.Context, tabID string, opts EmulateOptions) error {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return err
	}
	return cl.emulateTab(ctx, targetID, opts)
}

func (b *Bridge) CSS(ctx context.Context, tabID string, loc Locator) (CSSResult, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return CSSResult{}, err
	}
	return cl.cssTab(ctx, targetID, loc)
}

func (b *Bridge) HeapSnapshot(ctx context.Context, tabID, filePath string) (HeapSnapshotResult, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return HeapSnapshotResult{}, err
	}
	return cl.heapSnapshotTab(ctx, targetID, filePath)
}

func (b *Bridge) Logpoint(ctx context.Context, tabID, scriptURL string, line, column int, expr string) (string, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return "", err
	}
	return cl.logpointTab(ctx, targetID, scriptURL, line, column, expr)
}

func (b *Bridge) ScreencastStart(ctx context.Context, tabID string, maxWidth, maxHeight, quality int) error {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return err
	}
	return cl.screencastStartTab(ctx, targetID, maxWidth, maxHeight, quality)
}

func (b *Bridge) ScreencastStop(ctx context.Context, tabID string) ([]ScreencastFrame, error) {
	cl, targetID, err := b.resolveTab(ctx, tabID)
	if err != nil {
		return nil, err
	}
	return cl.screencastStopTab(ctx, targetID)
}

// RawCDP sends any CDP command either to the Browser Root (tabID == "") or to a
// specific tab/service-worker target (tabID != "").
func (b *Bridge) RawCDP(ctx context.Context, browserID, tabID, method string, params map[string]any) (any, error) {
	if strings.TrimSpace(method) == "" {
		return nil, errors.New("CDP method is required")
	}
	var cl *cdpClient
	var sessionID string
	var err error
	if tabID != "" {
		var targetID string
		cl, targetID, err = b.resolveTab(ctx, tabID)
		if err != nil {
			return nil, err
		}
		ts, err := cl.ensureAttached(ctx, targetID)
		if err != nil {
			return nil, err
		}
		sessionID = ts.sessionID
	} else {
		cl, err = b.resolveClient(ctx, browserID, false)
		if err != nil {
			return nil, err
		}
	}
	var out any
	if err := cl.Send(ctx, sessionID, method, params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *Bridge) authorized(request *http.Request) bool {
	provided, bearer := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return bearer && len(provided) == len(b.token) && subtle.ConstantTimeCompare([]byte(provided), []byte(b.token)) == 1
}

//go:embed all:extension
var extensionFiles embed.FS

// InstallExtension writes the thin Web Bridge + Grants Panel extension into directory.
func InstallExtension(directory string) (string, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(extensionFiles, "extension", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(abs, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(path, "extension"), "/")))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := extensionFiles.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", err
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
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return data, nil
}
