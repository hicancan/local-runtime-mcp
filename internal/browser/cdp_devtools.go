package browser

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var defaultTraceCategories = []string{
	"-*",
	"devtools.timeline",
	"disabled-by-default-devtools.timeline",
	"disabled-by-default-devtools.timeline.frame",
	"disabled-by-default-devtools.timeline.stack",
	"disabled-by-default-v8.cpu_profiler",
	"v8.execute",
	"blink.user_timing",
	"loading",
	"latencyInfo",
	"toplevel",
	"rail",
}

// TraceStartOptions configures browser-process-level Tracing.start.
type TraceStartOptions struct {
	BrowserID   string   `json:"browser_id,omitempty"`
	TabID       string   `json:"tab_id,omitempty"`
	Reload      bool     `json:"reload,omitempty"`
	IgnoreCache bool     `json:"ignore_cache,omitempty"`
	Categories  []string `json:"categories,omitempty"`
}

// TraceSummary extracts key Web Vitals & main-thread metrics directly from the raw trace.
type TraceSummary struct {
	FilePath         string             `json:"file_path"`
	Bytes            int64              `json:"bytes"`
	DurationMS       int64              `json:"duration_ms"`
	EventCount       int                `json:"event_count"`
	DataLossOccurred bool               `json:"data_loss_occurred,omitempty"`
	LongTaskCount    int                `json:"long_task_count"`
	MaxTaskMS        float64            `json:"max_task_ms,omitempty"`
	FCPMS            *float64           `json:"fcp_ms,omitempty"`
	LCPMS            *float64           `json:"lcp_ms,omitempty"`
	DCLMS            *float64           `json:"dcl_ms,omitempty"`
	LoadMS           *float64           `json:"load_ms,omitempty"`
	TopCategories    map[string]int     `json:"top_categories,omitempty"`
}

// EmulateOptions configures Chrome DevTools Emulation & Network throttling.
type EmulateOptions struct {
	Width             int               `json:"width,omitempty"`
	Height            int               `json:"height,omitempty"`
	DeviceScaleFactor float64           `json:"device_scale_factor,omitempty"`
	Mobile            bool              `json:"mobile,omitempty"`
	Touch             bool              `json:"touch,omitempty"`
	Landscape         bool              `json:"landscape,omitempty"`
	ClearViewport     bool              `json:"clear_viewport,omitempty"`
	CPUThrottlingRate float64           `json:"cpu_throttling_rate,omitempty"`
	NetworkPreset     string            `json:"network_preset,omitempty"` // Offline, Slow 3G, Fast 3G, Slow 4G, Fast 4G, Reset
	ColorScheme       string            `json:"color_scheme,omitempty"`   // dark, light, auto
	ReducedMotion     string            `json:"reduced_motion,omitempty"` // reduce, no-preference, auto
	ForcedColors      string            `json:"forced_colors,omitempty"`  // active, none, auto
	UserAgent         string            `json:"user_agent,omitempty"`
	Geolocation       *GeoLocation      `json:"geolocation,omitempty"`
	ExtraHeaders      map[string]string `json:"extra_headers,omitempty"`
}

type GeoLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Accuracy  float64 `json:"accuracy,omitempty"`
}

// NetworkFilter narrows passively captured network requests.
type NetworkFilter struct {
	ResourceTypes     []string `json:"resource_types,omitempty"`
	URLContains       string   `json:"url_contains,omitempty"`
	IncludePreserved  bool     `json:"include_preserved,omitempty"`
	FailedOnly        bool     `json:"failed_only,omitempty"`
	Limit             int      `json:"limit,omitempty"`
}

// ConsoleFilter narrows passively captured console messages & exceptions.
type ConsoleFilter struct {
	Levels []string `json:"levels,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

// NetworkBodyResult contains a lazily fetched response body.
type NetworkBodyResult struct {
	RequestID     string `json:"request_id"`
	Body          string `json:"body"`
	Base64Encoded bool   `json:"base64_encoded,omitempty"`
}

// CSSResult summarizes matched rules and computed styles for a DOM node.
type CSSResult struct {
	Selector string            `json:"selector,omitempty"`
	Inline   map[string]string `json:"inline,omitempty"`
	Matched  []CSSMatchedRule  `json:"matched,omitempty"`
	Computed map[string]string `json:"computed,omitempty"`
}

type CSSMatchedRule struct {
	Selector   string            `json:"selector"`
	Origin     string            `json:"origin,omitempty"`
	Properties map[string]string `json:"properties"`
}

// HeapSnapshotResult reports a streamed V8 heap snapshot saved to disk.
type HeapSnapshotResult struct {
	FilePath   string `json:"file_path"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

func (c *cdpClient) traceStart(ctx context.Context, opts TraceStartOptions, reloadTargetID string) error {
	c.traceMu.Lock()
	if c.traceActive {
		c.traceMu.Unlock()
		return errors.New("a browser-level trace is already active on this browser")
	}
	c.traceActive = true
	c.traceDone = make(chan traceCompleteEvent, 1)
	c.traceChunks = nil
	c.traceStarted = time.Now()
	c.traceTabID = opts.TabID
	c.traceMu.Unlock()

	cats := opts.Categories
	if len(cats) == 0 {
		cats = defaultTraceCategories
	}
	params := map[string]any{
		"transferMode": "ReturnAsStream",
		"traceConfig": map[string]any{
			"recordMode":         "recordUntilFull",
			"includedCategories": cats,
		},
	}
	// Sent directly on Browser Root target (sessionID == "") for full multi-process trace.
	if err := c.Send(ctx, "", "Tracing.start", params, nil); err != nil {
		c.traceMu.Lock()
		c.traceActive = false
		c.traceDone = nil
		c.traceMu.Unlock()
		return err
	}

	if opts.Reload && reloadTargetID != "" {
		ts, err := c.ensureAttached(ctx, reloadTargetID)
		if err != nil {
			return err
		}
		if opts.IgnoreCache {
			_ = c.Send(ctx, ts.sessionID, "Network.setCacheDisabled", map[string]any{"cacheDisabled": true}, nil)
		}
		if err := c.Send(ctx, ts.sessionID, "Page.reload", map[string]any{"ignoreCache": opts.IgnoreCache}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *cdpClient) traceStop(ctx context.Context, filePath string) (TraceSummary, error) {
	c.traceMu.Lock()
	if !c.traceActive {
		c.traceMu.Unlock()
		return TraceSummary{}, errors.New("no active browser trace to stop")
	}
	doneCh := c.traceDone
	started := c.traceStarted
	c.traceMu.Unlock()

	defer func() {
		c.traceMu.Lock()
		c.traceActive = false
		c.traceDone = nil
		c.traceChunks = nil
		c.traceMu.Unlock()
	}()

	if err := c.Send(ctx, "", "Tracing.end", nil, nil); err != nil {
		return TraceSummary{}, err
	}

	var completed traceCompleteEvent
	select {
	case completed = <-doneCh:
	case <-ctx.Done():
		return TraceSummary{}, ctx.Err()
	case <-time.After(30 * time.Second):
		return TraceSummary{}, errors.New("timed out waiting for Tracing.tracingComplete")
	}

	if strings.TrimSpace(filePath) == "" {
		filePath = filepath.Join(os.TempDir(), fmt.Sprintf("lrmcp-trace-%d.json", time.Now().UnixMilli()))
	} else {
		var err error
		filePath, err = filepath.Abs(filePath)
		if err != nil {
			return TraceSummary{}, err
		}
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return TraceSummary{}, err
		}
	}

	f, err := os.Create(filePath)
	if err != nil {
		return TraceSummary{}, err
	}
	bw := bufio.NewWriterSize(f, 256<<10)
	var written int64

	if completed.Stream != "" {
		for {
			var chunk struct {
				Data          string `json:"data"`
				EOF           bool   `json:"eof"`
				Base64Encoded bool   `json:"base64Encoded"`
			}
			if err := c.Send(ctx, "", "IO.read", map[string]any{
				"handle": completed.Stream,
				"size":   1 << 20,
			}, &chunk); err != nil {
				_ = f.Close()
				return TraceSummary{}, err
			}
			if chunk.Data != "" {
				var raw []byte
				if chunk.Base64Encoded {
					raw, err = base64.StdEncoding.DecodeString(chunk.Data)
					if err != nil {
						_ = f.Close()
						return TraceSummary{}, err
					}
				} else {
					raw = []byte(chunk.Data)
				}
				n, werr := bw.Write(raw)
				written += int64(n)
				if werr != nil {
					_ = f.Close()
					return TraceSummary{}, werr
				}
			}
			if chunk.EOF {
				break
			}
		}
		_ = c.Send(ctx, "", "IO.close", map[string]any{"handle": completed.Stream}, nil)
	} else {
		c.traceMu.Lock()
		chunks := append([]json.RawMessage(nil), c.traceChunks...)
		c.traceMu.Unlock()
		n, _ := bw.WriteString(`{"traceEvents":[`)
		written += int64(n)
		for i, raw := range chunks {
			if i > 0 {
				_ = bw.WriteByte(',')
				written++
			}
			m, _ := bw.Write(raw)
			written += int64(m)
		}
		n2, _ := bw.WriteString(`]}`)
		written += int64(n2)
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return TraceSummary{}, err
	}
	if err := f.Close(); err != nil {
		return TraceSummary{}, err
	}

	summary := analyzeTraceFile(filePath)
	summary.FilePath = filePath
	summary.Bytes = written
	summary.DurationMS = time.Since(started).Milliseconds()
	summary.DataLossOccurred = completed.DataLossOccurred
	return summary, nil
}

func analyzeTraceFile(path string) TraceSummary {
	out := TraceSummary{TopCategories: map[string]int{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var traceDoc struct {
		TraceEvents []struct {
			Name string  `json:"name"`
			Cat  string  `json:"cat"`
			Ph   string  `json:"ph"`
			TS   float64 `json:"ts"`
			Dur  float64 `json:"dur"`
		} `json:"traceEvents"`
	}
	if err := json.Unmarshal(data, &traceDoc); err != nil {
		// Some Chromium builds stream raw array instead of {"traceEvents":[...]}
		_ = json.Unmarshal(data, &traceDoc.TraceEvents)
	}
	out.EventCount = len(traceDoc.TraceEvents)
	var navStart float64
	for _, ev := range traceDoc.TraceEvents {
		if ev.Name == "navigationStart" && (navStart == 0 || ev.TS < navStart) {
			navStart = ev.TS
		}
	}
	for _, ev := range traceDoc.TraceEvents {
		if ev.Cat != "" {
			firstCat := strings.Split(ev.Cat, ",")[0]
			out.TopCategories[firstCat]++
		}
		if ev.Name == "RunTask" && ev.Dur >= 50_000 {
			out.LongTaskCount++
			ms := ev.Dur / 1000.0
			if ms > out.MaxTaskMS {
				out.MaxTaskMS = ms
			}
		}
		if navStart > 0 && ev.TS >= navStart {
			relMS := (ev.TS - navStart) / 1000.0
			switch ev.Name {
			case "firstContentfulPaint":
				if out.FCPMS == nil {
					v := relMS
					out.FCPMS = &v
				}
			case "largestContentfulPaint::Candidate":
				v := relMS
				out.LCPMS = &v
			case "domContentLoadedEventEnd":
				v := relMS
				out.DCLMS = &v
			case "loadEventEnd":
				v := relMS
				out.LoadMS = &v
			}
		}
	}
	return out
}

func (c *cdpClient) networkTab(ctx context.Context, targetID string, filter NetworkFilter) ([]NetworkEntry, error) {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return nil, err
	}
	typeSet := map[string]bool{}
	for _, rt := range filter.ResourceTypes {
		typeSet[strings.ToLower(strings.TrimSpace(rt))] = true
	}
	needle := strings.ToLower(strings.TrimSpace(filter.URLContains))
	limit := filter.Limit
	if limit <= 0 || limit > maxNetworkEntries {
		limit = 200
	}

	ts.mu.Lock()
	curNav := ts.navSeq
	out := make([]NetworkEntry, 0, len(ts.network))
	for _, item := range ts.network {
		if !filter.IncludePreserved && item.NavigationSeq != curNav {
			continue
		}
		if filter.FailedOnly && !item.Failed && item.Status < 400 {
			continue
		}
		if len(typeSet) > 0 && !typeSet[item.ResourceType] {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(item.URL), needle) {
			continue
		}
		out = append(out, *item)
	}
	ts.mu.Unlock()
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (c *cdpClient) networkBodyTab(ctx context.Context, targetID, requestID string) (NetworkBodyResult, error) {
	if strings.TrimSpace(requestID) == "" {
		return NetworkBodyResult{}, errors.New("request_id is required")
	}
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return NetworkBodyResult{}, err
	}
	var res struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := c.Send(ctx, ts.sessionID, "Network.getResponseBody", map[string]any{"requestId": requestID}, &res); err != nil {
		return NetworkBodyResult{}, err
	}
	return NetworkBodyResult{RequestID: requestID, Body: res.Body, Base64Encoded: res.Base64Encoded}, nil
}

func (c *cdpClient) consoleTab(ctx context.Context, targetID string, filter ConsoleFilter) ([]ConsoleEntry, error) {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return nil, err
	}
	levelSet := map[string]bool{}
	for _, lvl := range filter.Levels {
		levelSet[strings.ToLower(strings.TrimSpace(lvl))] = true
	}
	limit := filter.Limit
	if limit <= 0 || limit > maxConsoleEntries {
		limit = 200
	}
	ts.mu.Lock()
	out := make([]ConsoleEntry, 0, len(ts.console))
	for _, entry := range ts.console {
		if len(levelSet) > 0 && !levelSet[strings.ToLower(entry.Level)] {
			continue
		}
		out = append(out, entry)
	}
	ts.mu.Unlock()
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (c *cdpClient) emulateTab(ctx context.Context, targetID string, opts EmulateOptions) error {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return err
	}
	if opts.ClearViewport {
		if err := c.Send(ctx, ts.sessionID, "Emulation.clearDeviceMetricsOverride", nil, nil); err != nil {
			return err
		}
	} else if opts.Width > 0 && opts.Height > 0 {
		dpr := opts.DeviceScaleFactor
		if dpr <= 0 {
			dpr = 1
		}
		params := map[string]any{
			"width":             opts.Width,
			"height":            opts.Height,
			"deviceScaleFactor": dpr,
			"mobile":            opts.Mobile,
		}
		if opts.Landscape {
			params["screenOrientation"] = map[string]any{"type": "landscapePrimary", "angle": 90}
		}
		if err := c.Send(ctx, ts.sessionID, "Emulation.setDeviceMetricsOverride", params, nil); err != nil {
			return err
		}
		if opts.Touch {
			_ = c.Send(ctx, ts.sessionID, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true}, nil)
		}
	}
	if opts.CPUThrottlingRate >= 1 {
		if err := c.Send(ctx, ts.sessionID, "Emulation.setCPUThrottlingRate", map[string]any{"rate": opts.CPUThrottlingRate}, nil); err != nil {
			return err
		}
	}
	if opts.NetworkPreset != "" {
		netParams := networkPresetParams(opts.NetworkPreset)
		if err := c.Send(ctx, ts.sessionID, "Network.emulateNetworkConditions", netParams, nil); err != nil {
			return err
		}
	}
	var features []map[string]string
	if opts.ColorScheme != "" && opts.ColorScheme != "auto" {
		features = append(features, map[string]string{"name": "prefers-color-scheme", "value": opts.ColorScheme})
	}
	if opts.ReducedMotion != "" && opts.ReducedMotion != "auto" {
		features = append(features, map[string]string{"name": "prefers-reduced-motion", "value": opts.ReducedMotion})
	}
	if opts.ForcedColors != "" && opts.ForcedColors != "auto" {
		features = append(features, map[string]string{"name": "forced-colors", "value": opts.ForcedColors})
	}
	if len(features) > 0 || opts.ColorScheme == "auto" || opts.ReducedMotion == "auto" || opts.ForcedColors == "auto" {
		if err := c.Send(ctx, ts.sessionID, "Emulation.setEmulatedMedia", map[string]any{"features": features}, nil); err != nil {
			return err
		}
	}
	if opts.UserAgent != "" {
		if err := c.Send(ctx, ts.sessionID, "Emulation.setUserAgentOverride", map[string]any{"userAgent": opts.UserAgent}, nil); err != nil {
			return err
		}
	}
	if opts.Geolocation != nil {
		acc := opts.Geolocation.Accuracy
		if acc <= 0 {
			acc = 50
		}
		if err := c.Send(ctx, ts.sessionID, "Emulation.setGeolocationOverride", map[string]any{
			"latitude":  opts.Geolocation.Latitude,
			"longitude": opts.Geolocation.Longitude,
			"accuracy":  acc,
		}, nil); err != nil {
			return err
		}
	}
	if opts.ExtraHeaders != nil {
		if err := c.Send(ctx, ts.sessionID, "Network.setExtraHTTPHeaders", map[string]any{"headers": opts.ExtraHeaders}, nil); err != nil {
			return err
		}
	}
	return nil
}

func networkPresetParams(preset string) map[string]any {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "offline":
		return map[string]any{"offline": true, "latency": 0, "downloadThroughput": 0, "uploadThroughput": 0}
	case "slow 3g", "slow3g":
		return map[string]any{"offline": false, "latency": 2000, "downloadThroughput": 50000, "uploadThroughput": 50000}
	case "fast 3g", "fast3g":
		return map[string]any{"offline": false, "latency": 562, "downloadThroughput": 180000, "uploadThroughput": 84375}
	case "slow 4g", "slow4g":
		return map[string]any{"offline": false, "latency": 150, "downloadThroughput": 450000, "uploadThroughput": 187500}
	case "fast 4g", "fast4g":
		return map[string]any{"offline": false, "latency": 20, "downloadThroughput": 1125000, "uploadThroughput": 1125000}
	default:
		return map[string]any{"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1}
	}
}

func (c *cdpClient) cssTab(ctx context.Context, targetID string, loc Locator) (CSSResult, error) {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return CSSResult{}, err
	}
	target, err := c.resolveTarget(ctx, ts, loc)
	if err != nil {
		return CSSResult{}, err
	}
	if err := c.Send(ctx, target.SessionID, "CSS.enable", nil, nil); err != nil {
		return CSSResult{}, err
	}
	var pushed struct {
		NodeIDs []int `json:"nodeIds"`
	}
	if err := c.Send(ctx, target.SessionID, "DOM.pushNodesByBackendIdsToFrontend", map[string]any{
		"backendNodeIds": []int{target.BackendNodeID},
	}, &pushed); err != nil || len(pushed.NodeIDs) == 0 || pushed.NodeIDs[0] == 0 {
		return CSSResult{}, fmt.Errorf("resolve DOM node for CSS inspection: %w", err)
	}
	nodeID := pushed.NodeIDs[0]

	var computed struct {
		ComputedStyle []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"computedStyle"`
	}
	_ = c.Send(ctx, target.SessionID, "CSS.getComputedStyleForNode", map[string]any{"nodeId": nodeID}, &computed)

	var matched struct {
		InlineStyle *struct {
			CSSProperties []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cssProperties"`
		} `json:"inlineStyle"`
		MatchedCSSRules []struct {
			Rule struct {
				Origin       string `json:"origin"`
				SelectorList struct {
					Text string `json:"text"`
				} `json:"selectorList"`
				Style struct {
					CSSProperties []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"cssProperties"`
				} `json:"style"`
			} `json:"rule"`
		} `json:"matchedCSSRules"`
	}
	_ = c.Send(ctx, target.SessionID, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": nodeID}, &matched)

	out := CSSResult{
		Selector: loc.CSS,
		Inline:   map[string]string{},
		Computed: map[string]string{},
	}
	if matched.InlineStyle != nil {
		for _, p := range matched.InlineStyle.CSSProperties {
			if p.Name != "" && p.Value != "" {
				out.Inline[p.Name] = p.Value
			}
		}
	}
	for _, mr := range matched.MatchedCSSRules {
		if mr.Rule.Origin == "user-agent" {
			continue
		}
		props := map[string]string{}
		for _, p := range mr.Rule.Style.CSSProperties {
			if p.Name != "" && p.Value != "" {
				props[p.Name] = p.Value
			}
		}
		if len(props) > 0 {
			out.Matched = append(out.Matched, CSSMatchedRule{
				Selector:   mr.Rule.SelectorList.Text,
				Origin:     mr.Rule.Origin,
				Properties: props,
			})
		}
	}
	keyComputed := map[string]bool{
		"display": true, "position": true, "width": true, "height": true,
		"margin": true, "padding": true, "color": true, "background-color": true,
		"font-family": true, "font-size": true, "font-weight": true, "line-height": true,
		"opacity": true, "visibility": true, "z-index": true, "overflow": true,
		"flex-direction": true, "grid-template-columns": true,
	}
	for _, p := range computed.ComputedStyle {
		if keyComputed[p.Name] || strings.HasPrefix(p.Name, "--") {
			out.Computed[p.Name] = p.Value
		}
	}
	return out, nil
}

func (c *cdpClient) heapSnapshotTab(ctx context.Context, targetID, filePath string) (HeapSnapshotResult, error) {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return HeapSnapshotResult{}, err
	}
	if strings.TrimSpace(filePath) == "" {
		filePath = filepath.Join(os.TempDir(), fmt.Sprintf("lrmcp-heap-%d.heapsnapshot", time.Now().UnixMilli()))
	} else {
		filePath, err = filepath.Abs(filePath)
		if err != nil {
			return HeapSnapshotResult{}, err
		}
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return HeapSnapshotResult{}, err
		}
	}
	f, err := os.Create(filePath)
	if err != nil {
		return HeapSnapshotResult{}, err
	}
	bw := bufio.NewWriterSize(f, 256<<10)
	var total int64
	var writeErr error

	c.heapMu.Lock()
	c.heapSinks[ts.sessionID] = func(chunk string) {
		if writeErr != nil {
			return
		}
		n, err := bw.WriteString(chunk)
		total += int64(n)
		if err != nil {
			writeErr = err
		}
	}
	c.heapMu.Unlock()
	defer func() {
		c.heapMu.Lock()
		delete(c.heapSinks, ts.sessionID)
		c.heapMu.Unlock()
	}()

	start := time.Now()
	_ = c.Send(ctx, ts.sessionID, "HeapProfiler.enable", nil, nil)
	if err := c.Send(ctx, ts.sessionID, "HeapProfiler.takeHeapSnapshot", map[string]any{"reportProgress": false}, nil); err != nil {
		_ = f.Close()
		return HeapSnapshotResult{}, err
	}
	if writeErr != nil {
		_ = f.Close()
		return HeapSnapshotResult{}, writeErr
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return HeapSnapshotResult{}, err
	}
	if err := f.Close(); err != nil {
		return HeapSnapshotResult{}, err
	}
	return HeapSnapshotResult{
		FilePath:   filePath,
		Bytes:      total,
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
}

func (c *cdpClient) logpointTab(ctx context.Context, targetID, scriptURL string, line, column int, expr string) (string, error) {
	if strings.TrimSpace(scriptURL) == "" || strings.TrimSpace(expr) == "" {
		return "", errors.New("url and expression are required for logpoint")
	}
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return "", err
	}
	if err := c.Send(ctx, ts.sessionID, "Debugger.enable", nil, nil); err != nil {
		return "", err
	}
	// Chrome DevTools logpoint convention: evaluate console.log(...) and return false so execution never pauses.
	condition := fmt.Sprintf("(console.log(%s), false)", expr)
	var res struct {
		BreakpointID string `json:"breakpointId"`
	}
	if err := c.Send(ctx, ts.sessionID, "Debugger.setBreakpointByUrl", map[string]any{
		"url":          scriptURL,
		"lineNumber":   max(0, line-1),
		"columnNumber": max(0, column),
		"condition":    condition,
	}, &res); err != nil {
		return "", err
	}
	return res.BreakpointID, nil
}

func (c *cdpClient) screencastStartTab(ctx context.Context, targetID string, maxWidth, maxHeight, quality int) error {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return err
	}
	if quality <= 0 || quality > 100 {
		quality = 75
	}
	ts.mu.Lock()
	ts.screencastOn = true
	ts.screencastFrames = nil
	ts.mu.Unlock()
	params := map[string]any{"format": "jpeg", "quality": quality}
	if maxWidth > 0 {
		params["maxWidth"] = maxWidth
	}
	if maxHeight > 0 {
		params["maxHeight"] = maxHeight
	}
	return c.Send(ctx, ts.sessionID, "Page.startScreencast", params, nil)
}

func (c *cdpClient) screencastStopTab(ctx context.Context, targetID string) ([]ScreencastFrame, error) {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return nil, err
	}
	_ = c.Send(ctx, ts.sessionID, "Page.stopScreencast", nil, nil)
	ts.mu.Lock()
	ts.screencastOn = false
	frames := append([]ScreencastFrame(nil), ts.screencastFrames...)
	ts.screencastFrames = nil
	ts.mu.Unlock()
	return frames, nil
}

func (c *cdpClient) targetMeta(ctx context.Context, targetID string) (title, pageURL string) {
	var res struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Title    string `json:"title"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := c.Send(ctx, "", "Target.getTargets", nil, &res); err == nil {
		for _, info := range res.TargetInfos {
			if info.TargetID == targetID {
				return info.Title, info.URL
			}
		}
	}
	return "", ""
}
