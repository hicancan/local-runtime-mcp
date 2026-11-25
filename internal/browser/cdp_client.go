package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

const (
	maxCDPPayloadBytes = 128 << 20
	maxNetworkEntries  = 500
	maxConsoleEntries  = 500
	maxScreencastBuf   = 20
	maxTraceEvents     = 250_000
)

type cdpRequest struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type cdpEnvelope struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpError) Error() string {
	if e == nil {
		return ""
	}
	if e.Data != "" {
		return fmt.Sprintf("CDP error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// NetworkEntry represents one observed HTTP/WebSocket request captured passively on a tab.
type NetworkEntry struct {
	RequestID       string            `json:"request_id"`
	LoaderID        string            `json:"-"`
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	ResourceType    string            `json:"resource_type,omitempty"`
	Status          int               `json:"status,omitempty"`
	StatusText      string            `json:"status_text,omitempty"`
	MIMEType        string            `json:"mime_type,omitempty"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	EncodedBytes    float64           `json:"encoded_bytes,omitempty"`
	Failed          bool              `json:"failed,omitempty"`
	ErrorText       string            `json:"error_text,omitempty"`
	NavigationSeq   int               `json:"navigation_seq"`
	StartedAt       int64             `json:"started_at"`
	FinishedAt      int64             `json:"finished_at,omitempty"`
	DurationMS      int64             `json:"duration_ms,omitempty"`
}

// ConsoleEntry represents one console API call, browser log entry, or uncaught exception.
type ConsoleEntry struct {
	ID        int64  `json:"id"`
	Level     string `json:"level"`
	Source    string `json:"source"`
	Text      string `json:"text"`
	URL       string `json:"url,omitempty"`
	Line      int    `json:"line,omitempty"`
	Column    int    `json:"column,omitempty"`
	Stack     string `json:"stack,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// ScreencastFrame stores one captured frame from Page.startScreencast.
type ScreencastFrame struct {
	Seq       int64  `json:"seq"`
	Timestamp int64  `json:"timestamp"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	MIMEType  string `json:"mime_type"`
	Data      []byte `json:"-"`
}

type axTargetRef struct {
	SessionID     string
	BackendNodeID int
}

type viewportCapture struct {
	ID     string
	Width  int
	Height int
	PageX  float64
	PageY  float64
}

type tabSession struct {
	mu               sync.Mutex
	targetID         string
	sessionID        string
	epoch            string
	navSeq           int
	childSessions    map[string]bool
	lastAXLines      []string
	snapshots        map[string]map[string]axTargetRef
	snapshotOrder    []string
	snapshotSeq      int64
	viewport         *viewportCapture
	openDialog       bool
	dialogWaiters    []chan struct{}
	loadWaiters      []chan struct{}
	network          []*NetworkEntry
	networkByID      map[string]*NetworkEntry
	console          []ConsoleEntry
	consoleSeq       int64
	screencastOn     bool
	screencastSeq    int64
	screencastFrames []ScreencastFrame
	emulationActive  bool
}

func newTabSession(targetID, sessionID string) *tabSession {
	return &tabSession{
		targetID:      targetID,
		sessionID:     sessionID,
		epoch:         fmt.Sprintf("e%x", time.Now().UnixNano()),
		childSessions: map[string]bool{},
		snapshots:     map[string]map[string]axTargetRef{},
		networkByID:   map[string]*NetworkEntry{},
	}
}

func (ts *tabSession) bumpEpochLocked() {
	ts.epoch = fmt.Sprintf("e%x", time.Now().UnixNano())
	ts.snapshots = map[string]map[string]axTargetRef{}
	ts.snapshotOrder = nil
	ts.viewport = nil
}

// cdpClient owns one persistent browser-level CDP WebSocket connection with
// full concurrent request multiplexing, flat tab/OOPIF sessions, DevTools ring
// buffers, and browser-process-level Tracing.
type cdpClient struct {
	id          string
	label       string
	wsURL       string
	fingerprint string

	ctx    context.Context
	cancel context.CancelFunc
	conn   *websocket.Conn
	writeMu sync.Mutex

	seq     atomic.Int64
	pendMu  sync.Mutex
	pending map[int64]chan cdpEnvelope

	stateMu        sync.Mutex
	closed         bool
	closeErr       error
	productVersion string
	tabs           map[string]*tabSession // targetID -> tabSession
	bySession      map[string]*tabSession // sessionID -> tabSession
	attaching      map[string]chan struct{}
	attached       map[string]bool

	// Browser-level tracing state
	traceMu            sync.Mutex
	traceActive        bool
	traceDone          chan traceCompleteEvent
	traceChunks        []json.RawMessage
	traceStarted       time.Time
	traceTabID         string
	traceTargetID      string
	traceCacheDisabled bool

	// Heap snapshot chunk sink (keyed by sessionID)
	heapMu    sync.Mutex
	heapSinks map[string]func(string)

	done chan struct{}
}

type traceCompleteEvent struct {
	Stream            string `json:"stream"`
	TraceFormat       string `json:"traceFormat"`
	StreamCompression string `json:"streamCompression"`
	DataLossOccurred  bool   `json:"dataLossOccurred"`
}

func dialCDP(parent context.Context, id, label, wsURL, fingerprint string) (*cdpClient, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("parse CDP URL: %w", err)
	}
	origin := "http://" + parsed.Host
	wsCfg, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		return nil, err
	}
	wsCfg.Header = http.Header{}
	wsCfg.Dialer = nil

	// Dial with context support so canceling the dial does not leak goroutines.
	type dialRes struct {
		conn *websocket.Conn
		err  error
	}
	ch := make(chan dialRes, 1)
	go func() {
		conn, err := websocket.DialConfig(wsCfg)
		ch <- dialRes{conn: conn, err: err}
	}()
	var conn *websocket.Conn
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		conn = res.conn
	case <-parent.Done():
		go func() {
			if res := <-ch; res.conn != nil {
				_ = res.conn.Close()
			}
		}()
		return nil, parent.Err()
	}
	conn.MaxPayloadBytes = maxCDPPayloadBytes

	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	c := &cdpClient{
		id:          id,
		label:       label,
		wsURL:       wsURL,
		fingerprint: fingerprint,
		ctx:         ctx,
		cancel:      cancel,
		conn:        conn,
		pending:     map[int64]chan cdpEnvelope{},
		tabs:        map[string]*tabSession{},
		bySession:   map[string]*tabSession{},
		attaching:   map[string]chan struct{}{},
		attached:    map[string]bool{},
		heapSinks:   map[string]func(string){},
		done:        make(chan struct{}),
	}
	go c.readLoop()

	verCtx, verCancel := context.WithTimeout(ctx, 5*time.Second)
	defer verCancel()
	var ver struct {
		Product   string `json:"product"`
		UserAgent string `json:"userAgent"`
	}
	if err := c.Send(verCtx, "", "Browser.getVersion", nil, &ver); err != nil {
		c.Close()
		return nil, fmt.Errorf("Browser.getVersion failed: %w", err)
	}
	c.stateMu.Lock()
	if ver.UserAgent != "" {
		c.productVersion = ver.UserAgent
	} else {
		c.productVersion = ver.Product
	}
	c.stateMu.Unlock()
	return c, nil
}

func (c *cdpClient) Close() {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return
	}
	c.closed = true
	c.stateMu.Unlock()
	c.cancel()
	_ = c.conn.Close()
	<-c.done
}

func (c *cdpClient) Alive() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return !c.closed
}

func (c *cdpClient) ProductVersion() string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.productVersion
}

func (c *cdpClient) sendRaw(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := c.seq.Add(1)
	respCh := make(chan cdpEnvelope, 1)

	c.pendMu.Lock()
	c.stateMu.Lock()
	closed := c.closed
	c.stateMu.Unlock()
	if closed {
		c.pendMu.Unlock()
		return nil, errors.New("CDP browser connection is closed")
	}
	c.pending[id] = respCh
	c.pendMu.Unlock()

	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	payload, err := json.Marshal(cdpRequest{
		ID:        id,
		Method:    method,
		Params:    params,
		SessionID: sessionID,
	})
	if err != nil {
		return nil, err
	}

	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	writeErr := websocket.Message.Send(c.conn, string(payload))
	c.writeMu.Unlock()
	if writeErr != nil {
		return nil, fmt.Errorf("CDP send %s: %w", method, writeErr)
	}

	select {
	case env, ok := <-respCh:
		if !ok {
			return nil, errors.New("CDP browser connection closed while awaiting response")
		}
		if env.Error != nil {
			return nil, env.Error
		}
		return env.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("CDP browser connection closed")
	}
}

func (c *cdpClient) Send(ctx context.Context, sessionID, method string, params any, out any) error {
	raw, err := c.sendRaw(ctx, sessionID, method, params)
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode CDP %s result: %w", method, err)
		}
	}
	return nil
}

func (c *cdpClient) readLoop() {
	defer func() {
		c.stateMu.Lock()
		c.closed = true
		c.stateMu.Unlock()
		c.pendMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pendMu.Unlock()
		close(c.done)
	}()

	for {
		var raw []byte
		if err := websocket.Message.Receive(c.conn, &raw); err != nil {
			c.stateMu.Lock()
			c.closeErr = err
			c.stateMu.Unlock()
			return
		}
		var env cdpEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.ID > 0 {
			c.pendMu.Lock()
			ch := c.pending[env.ID]
			delete(c.pending, env.ID)
			c.pendMu.Unlock()
			if ch != nil {
				ch <- env
			}
			continue
		}
		if env.Method != "" {
			c.handleEvent(env)
		}
	}
}

func (c *cdpClient) handleEvent(env cdpEnvelope) {
	// Browser-level events (sessionId == "") and lifecycle detaches
	switch env.Method {
	case "Tracing.dataCollected":
		var p struct {
			Value []json.RawMessage `json:"value"`
		}
		if json.Unmarshal(env.Params, &p) == nil && len(p.Value) > 0 {
			c.traceMu.Lock()
			if c.traceActive && len(c.traceChunks) < maxTraceEvents {
				rem := maxTraceEvents - len(c.traceChunks)
				if len(p.Value) > rem {
					p.Value = p.Value[:rem]
				}
				c.traceChunks = append(c.traceChunks, p.Value...)
			}
			c.traceMu.Unlock()
		}
		return
	case "Tracing.tracingComplete":
		var p traceCompleteEvent
		_ = json.Unmarshal(env.Params, &p)
		c.traceMu.Lock()
		ch := c.traceDone
		c.traceMu.Unlock()
		if ch != nil {
			select {
			case ch <- p:
			default:
			}
		}
		return
	case "HeapProfiler.addHeapSnapshotChunk":
		var p struct {
			Chunk string `json:"chunk"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			c.heapMu.Lock()
			sink := c.heapSinks[env.SessionID]
			c.heapMu.Unlock()
			if sink != nil {
				sink(p.Chunk)
			}
		}
		return
	case "Target.targetDestroyed":
		var p struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.TargetID != "" {
			c.stateMu.Lock()
			ts := c.tabs[p.TargetID]
			delete(c.tabs, p.TargetID)
			delete(c.attached, p.TargetID)
			if ts != nil {
				delete(c.bySession, ts.sessionID)
				ts.mu.Lock()
				for child := range ts.childSessions {
					delete(c.bySession, child)
				}
				ts.mu.Unlock()
			}
			c.stateMu.Unlock()
		}
		return
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
			TargetID  string `json:"targetId"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.SessionID != "" {
			c.stateMu.Lock()
			ts := c.bySession[p.SessionID]
			if ts == nil && p.TargetID != "" {
				ts = c.tabs[p.TargetID]
			}
			if ts != nil && ts.sessionID == p.SessionID {
				// Main tab session detached (crash / close / DevTools detach): evict cleanly so next call re-attaches.
				delete(c.tabs, ts.targetID)
				delete(c.attached, ts.targetID)
				delete(c.bySession, ts.sessionID)
				ts.mu.Lock()
				for child := range ts.childSessions {
					delete(c.bySession, child)
				}
				ts.mu.Unlock()
				c.stateMu.Unlock()
				return
			}
			delete(c.bySession, p.SessionID)
			c.stateMu.Unlock()
			if ts != nil {
				ts.mu.Lock()
				delete(ts.childSessions, p.SessionID)
				ts.bumpEpochLocked()
				ts.mu.Unlock()
			}
		}
		return
	}

	c.stateMu.Lock()
	ts := c.bySession[env.SessionID]
	c.stateMu.Unlock()
	if ts == nil {
		return
	}

	switch env.Method {
	case "Target.attachedToTarget":
		var p struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				Type string `json:"type"`
			} `json:"targetInfo"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.SessionID != "" {
			ts.mu.Lock()
			ts.childSessions[p.SessionID] = true
			ts.mu.Unlock()
			c.stateMu.Lock()
			c.bySession[p.SessionID] = ts
			c.stateMu.Unlock()
			go func(childSess string) {
				ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
				defer cancel()
				_ = c.Send(ctx, childSess, "Target.setAutoAttach", map[string]any{
					"autoAttach":             true,
					"waitForDebuggerOnStart": false,
					"flatten":                true,
					"filter":                 []map[string]any{{"type": "iframe", "exclude": false}},
				}, nil)
			}(p.SessionID)
		}
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.SessionID != "" {
			ts.mu.Lock()
			delete(ts.childSessions, p.SessionID)
			ts.bumpEpochLocked()
			ts.mu.Unlock()
			c.stateMu.Lock()
			delete(c.bySession, p.SessionID)
			c.stateMu.Unlock()
		}
	case "Page.frameNavigated":
		var p struct {
			Frame struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.Frame.ParentID == "" {
			ts.mu.Lock()
			ts.navSeq++
			ts.lastAXLines = nil
			ts.bumpEpochLocked()
			// Promote main-document navigation request(s) sharing this loaderId
			// (which fire Network.requestWillBeSent before Page.frameNavigated commits)
			// into the new navigation sequence so tab.network() always includes the Document.
			if p.Frame.LoaderID != "" {
				for _, e := range ts.network {
					if e.LoaderID == p.Frame.LoaderID {
						e.NavigationSeq = ts.navSeq
					}
				}
			}
			// Retain up to 3 navigations of network requests.
			minNav := ts.navSeq - 3
			if minNav > 0 && len(ts.network) > 0 {
				kept := ts.network[:0]
				for _, e := range ts.network {
					if e.NavigationSeq >= minNav {
						kept = append(kept, e)
					} else {
						delete(ts.networkByID, e.RequestID)
					}
				}
				clear(ts.network[len(kept):])
				ts.network = kept
			}
			ts.mu.Unlock()
		}
	case "Page.loadEventFired":
		ts.mu.Lock()
		waiters := ts.loadWaiters
		ts.loadWaiters = nil
		ts.mu.Unlock()
		for _, ch := range waiters {
			close(ch)
		}
	case "Page.javascriptDialogOpening":
		ts.mu.Lock()
		ts.openDialog = true
		waiters := ts.dialogWaiters
		ts.dialogWaiters = nil
		ts.mu.Unlock()
		for _, ch := range waiters {
			close(ch)
		}
	case "Page.javascriptDialogClosed":
		ts.mu.Lock()
		ts.openDialog = false
		ts.mu.Unlock()
	case "Network.requestWillBeSent":
		var p struct {
			RequestID string `json:"requestId"`
			LoaderID  string `json:"loaderId"`
			Type      string `json:"type"`
			Request   struct {
				URL     string            `json:"url"`
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
			} `json:"request"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.RequestID != "" {
			ts.mu.Lock()
			entry := &NetworkEntry{
				RequestID:      p.RequestID,
				LoaderID:       p.LoaderID,
				URL:            p.Request.URL,
				Method:         p.Request.Method,
				ResourceType:   strings.ToLower(p.Type),
				RequestHeaders: p.Request.Headers,
				NavigationSeq:  ts.navSeq,
				StartedAt:      time.Now().UnixMilli(),
			}
			ts.network = append(ts.network, entry)
			ts.networkByID[p.RequestID] = entry
			if len(ts.network) > maxNetworkEntries {
				dropped := ts.network[0]
				copy(ts.network, ts.network[1:])
				ts.network[len(ts.network)-1] = nil
				ts.network = ts.network[:len(ts.network)-1]
				if ts.networkByID[dropped.RequestID] == dropped {
					delete(ts.networkByID, dropped.RequestID)
				}
			}
			ts.mu.Unlock()
		}
	case "Network.responseReceived":
		var p struct {
			RequestID string `json:"requestId"`
			Type      string `json:"type"`
			Response  struct {
				Status     int               `json:"status"`
				StatusText string            `json:"statusText"`
				MIMEType   string            `json:"mimeType"`
				Headers    map[string]string `json:"headers"`
			} `json:"response"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.RequestID != "" {
			ts.mu.Lock()
			if entry := ts.networkByID[p.RequestID]; entry != nil {
				entry.Status = p.Response.Status
				entry.StatusText = p.Response.StatusText
				entry.MIMEType = p.Response.MIMEType
				entry.ResponseHeaders = p.Response.Headers
				if entry.ResourceType == "" && p.Type != "" {
					entry.ResourceType = strings.ToLower(p.Type)
				}
			}
			ts.mu.Unlock()
		}
	case "Network.loadingFinished":
		var p struct {
			RequestID         string  `json:"requestId"`
			EncodedDataLength float64 `json:"encodedDataLength"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.RequestID != "" {
			now := time.Now().UnixMilli()
			ts.mu.Lock()
			if entry := ts.networkByID[p.RequestID]; entry != nil {
				entry.EncodedBytes = p.EncodedDataLength
				entry.FinishedAt = now
				entry.DurationMS = now - entry.StartedAt
			}
			ts.mu.Unlock()
		}
	case "Network.loadingFailed":
		var p struct {
			RequestID string `json:"requestId"`
			ErrorText string `json:"errorText"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.RequestID != "" {
			now := time.Now().UnixMilli()
			ts.mu.Lock()
			if entry := ts.networkByID[p.RequestID]; entry != nil {
				entry.Failed = true
				entry.ErrorText = p.ErrorText
				entry.FinishedAt = now
				entry.DurationMS = now - entry.StartedAt
			}
			ts.mu.Unlock()
		}
	case "Runtime.consoleAPICalled":
		var p struct {
			Type string `json:"type"`
			Args []struct {
				Type        string `json:"type"`
				Value       any    `json:"value"`
				Description string `json:"description"`
			} `json:"args"`
			StackTrace *struct {
				CallFrames []struct {
					FunctionName string `json:"functionName"`
					URL          string `json:"url"`
					LineNumber   int    `json:"lineNumber"`
					ColumnNumber int    `json:"columnNumber"`
				} `json:"callFrames"`
			} `json:"stackTrace"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			parts := make([]string, 0, len(p.Args))
			for _, arg := range p.Args {
				if arg.Description != "" {
					parts = append(parts, arg.Description)
				} else if arg.Value != nil {
					parts = append(parts, fmt.Sprint(arg.Value))
				} else {
					parts = append(parts, arg.Type)
				}
			}
			entry := ConsoleEntry{
				Level:     p.Type,
				Source:    "console",
				Text:      strings.Join(parts, " "),
				Timestamp: time.Now().UnixMilli(),
			}
			if p.StackTrace != nil && len(p.StackTrace.CallFrames) > 0 {
				f := p.StackTrace.CallFrames[0]
				entry.URL = f.URL
				entry.Line = f.LineNumber + 1
				entry.Column = f.ColumnNumber + 1
			}
			ts.appendConsole(entry)
		}
	case "Runtime.exceptionThrown":
		var p struct {
			ExceptionDetails struct {
				Text       string `json:"text"`
				URL        string `json:"url"`
				LineNumber int    `json:"lineNumber"`
				ColNumber  int    `json:"columnNumber"`
				Exception  *struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			text := p.ExceptionDetails.Text
			if p.ExceptionDetails.Exception != nil && p.ExceptionDetails.Exception.Description != "" {
				text = p.ExceptionDetails.Exception.Description
			}
			ts.appendConsole(ConsoleEntry{
				Level:     "error",
				Source:    "exception",
				Text:      text,
				URL:       p.ExceptionDetails.URL,
				Line:      p.ExceptionDetails.LineNumber + 1,
				Column:    p.ExceptionDetails.ColNumber + 1,
				Timestamp: time.Now().UnixMilli(),
			})
		}
	case "Log.entryAdded":
		var p struct {
			Entry struct {
				Source     string `json:"source"`
				Level      string `json:"level"`
				Text       string `json:"text"`
				URL        string `json:"url"`
				LineNumber int    `json:"lineNumber"`
			} `json:"entry"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			ts.appendConsole(ConsoleEntry{
				Level:     p.Entry.Level,
				Source:    p.Entry.Source,
				Text:      p.Entry.Text,
				URL:       p.Entry.URL,
				Line:      p.Entry.LineNumber + 1,
				Timestamp: time.Now().UnixMilli(),
			})
		}
	case "Page.screencastFrame":
		var p struct {
			Data      string `json:"data"`
			SessionID int    `json:"sessionId"`
			Metadata  struct {
				DeviceWidth  float64 `json:"deviceWidth"`
				DeviceHeight float64 `json:"deviceHeight"`
			} `json:"metadata"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			if raw, err := decodeBase64(p.Data); err == nil {
				ts.mu.Lock()
				if ts.screencastOn {
					ts.screencastSeq++
					ts.screencastFrames = append(ts.screencastFrames, ScreencastFrame{
						Seq:       ts.screencastSeq,
						Timestamp: time.Now().UnixMilli(),
						Width:     int(p.Metadata.DeviceWidth),
						Height:    int(p.Metadata.DeviceHeight),
						MIMEType:  "image/jpeg",
						Data:      raw,
					})
					if len(ts.screencastFrames) > maxScreencastBuf {
						excess := len(ts.screencastFrames) - maxScreencastBuf
						copy(ts.screencastFrames, ts.screencastFrames[excess:])
						clear(ts.screencastFrames[maxScreencastBuf:])
						ts.screencastFrames = ts.screencastFrames[:maxScreencastBuf]
					}
				}
				ts.mu.Unlock()
			}
			go func(sess string, frameSess int) {
				ackCtx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
				defer cancel()
				_ = c.Send(ackCtx, sess, "Page.screencastFrameAck", map[string]any{"sessionId": frameSess}, nil)
			}(env.SessionID, p.SessionID)
		}
	}
}

func (ts *tabSession) subscribeLoad() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	ts.mu.Lock()
	ts.loadWaiters = append(ts.loadWaiters, ch)
	ts.mu.Unlock()
	cancel := func() {
		ts.mu.Lock()
		for i, w := range ts.loadWaiters {
			if w == ch {
				ts.loadWaiters = append(ts.loadWaiters[:i], ts.loadWaiters[i+1:]...)
				break
			}
		}
		ts.mu.Unlock()
	}
	return ch, cancel
}

func (ts *tabSession) appendConsole(entry ConsoleEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.consoleSeq++
	entry.ID = ts.consoleSeq
	if len(entry.Text) > 4096 {
		entry.Text = truncateUTF8Bytes(entry.Text, 4096) + "…"
	}
	ts.console = append(ts.console, entry)
	if len(ts.console) > maxConsoleEntries {
		excess := len(ts.console) - maxConsoleEntries
		copy(ts.console, ts.console[excess:])
		clear(ts.console[maxConsoleEntries:])
		ts.console = ts.console[:maxConsoleEntries]
	}
}

// ensureAttached attaches a flat CDP session to targetID and enables core
// DevTools domains (Page, Runtime, Log, Network, DOM, OOPIF auto-attach).
func (c *cdpClient) ensureAttached(ctx context.Context, targetID string) (*tabSession, error) {
	for {
		c.stateMu.Lock()
		if c.closed {
			c.stateMu.Unlock()
			return nil, errors.New("CDP browser connection is closed")
		}
		if ts := c.tabs[targetID]; ts != nil && c.attached[targetID] {
			c.stateMu.Unlock()
			return ts, nil
		}
		if waitCh := c.attaching[targetID]; waitCh != nil {
			c.stateMu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		waitCh := make(chan struct{})
		c.attaching[targetID] = waitCh
		c.stateMu.Unlock()

		var res struct {
			SessionID string `json:"sessionId"`
		}
		err := c.Send(ctx, "", "Target.attachToTarget", map[string]any{
			"targetId": targetID,
			"flatten":  true,
		}, &res)
		if err != nil {
			c.stateMu.Lock()
			delete(c.attaching, targetID)
			close(waitCh)
			c.stateMu.Unlock()
			return nil, err
		}

		ts := newTabSession(targetID, res.SessionID)
		c.stateMu.Lock()
		c.tabs[targetID] = ts
		c.bySession[res.SessionID] = ts
		c.stateMu.Unlock()

		// Enable passive observation domains cleanly in parallel.
		var wg sync.WaitGroup
		for _, item := range []struct {
			method string
			params any
		}{
			{"Page.enable", nil},
			{"Runtime.enable", nil},
			{"Log.enable", nil},
			{"Network.enable", map[string]any{"maxTotalBufferSize": 16 << 20, "maxResourceBufferSize": 4 << 20}},
			{"DOM.enable", nil},
			{"Target.setAutoAttach", map[string]any{
				"autoAttach":             true,
				"waitForDebuggerOnStart": false,
				"flatten":                true,
				"filter":                 []map[string]any{{"type": "iframe", "exclude": false}},
			}},
		} {
			wg.Add(1)
			go func(m string, p any) {
				defer wg.Done()
				enCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				_ = c.Send(enCtx, res.SessionID, m, p, nil)
			}(item.method, item.params)
		}
		wg.Wait()

		c.stateMu.Lock()
		c.attached[targetID] = true
		delete(c.attaching, targetID)
		close(waitCh)
		c.stateMu.Unlock()
		return ts, nil
	}
}

func (c *cdpClient) sendAllowDialog(ctx context.Context, ts *tabSession, sessionID, method string, params any, out any) error {
	ts.mu.Lock()
	if ts.openDialog {
		ts.mu.Unlock()
		return nil
	}
	waiter := make(chan struct{})
	ts.dialogWaiters = append(ts.dialogWaiters, waiter)
	ts.mu.Unlock()

	defer func() {
		ts.mu.Lock()
		for i, ch := range ts.dialogWaiters {
			if ch == waiter {
				ts.dialogWaiters = append(ts.dialogWaiters[:i], ts.dialogWaiters[i+1:]...)
				break
			}
		}
		ts.mu.Unlock()
	}()

	type rawRes struct {
		raw json.RawMessage
		err error
	}
	resCh := make(chan rawRes, 1)
	go func() {
		raw, err := c.sendRaw(ctx, sessionID, method, params)
		resCh <- rawRes{raw: raw, err: err}
	}()
	select {
	case res := <-resCh:
		if res.err != nil {
			return res.err
		}
		if out != nil && len(res.raw) > 0 {
			if err := json.Unmarshal(res.raw, out); err != nil {
				return fmt.Errorf("decode CDP %s result: %w", method, err)
			}
		}
		return nil
	case <-waiter:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
