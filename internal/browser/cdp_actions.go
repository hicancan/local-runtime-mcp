package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Locator describes a live semantic or CSS target inside a page.
type Locator struct {
	Role        string `json:"role,omitempty"`
	Name        string `json:"name,omitempty"`
	Exact       bool   `json:"exact,omitempty"`
	Text        string `json:"text,omitempty"`
	Label       string `json:"label,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	CSS         string `json:"css,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Nth         int    `json:"nth,omitempty"`
	TimeoutMS   int    `json:"timeout_ms,omitempty"`
}

func (l Locator) empty() bool {
	return l.Role == "" && l.Name == "" && l.Text == "" && l.Label == "" && l.Placeholder == "" && l.CSS == "" && l.Ref == ""
}

type resolvedPoint struct {
	SessionID string
	X         float64
	Y         float64
}

func (c *cdpClient) screenshotTab(ctx context.Context, tabID, targetID string, options ScreenshotOptions) ([]byte, ScreenshotInfo, error) {
	if options.FullPage && (options.ClipW != 0 || options.ClipH != 0 || options.ClipX != 0 || options.ClipY != 0) {
		return nil, ScreenshotInfo{}, errors.New("full_page and clip fields are mutually exclusive")
	}
	if (options.ClipW == 0) != (options.ClipH == 0) || options.ClipW < 0 || options.ClipH < 0 || options.ClipX < 0 || options.ClipY < 0 {
		return nil, ScreenshotInfo{}, errors.New("clip requires positive clip_width and clip_height with non-negative coordinates")
	}
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return nil, ScreenshotInfo{}, err
	}
	title, pageURL := c.targetMeta(ctx, targetID)

	ts.mu.Lock()
	epoch := ts.epoch
	sessID := ts.sessionID
	ts.mu.Unlock()

	var metrics struct {
		CSSVisualViewport *struct {
			PageX        float64 `json:"pageX"`
			PageY        float64 `json:"pageY"`
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssVisualViewport"`
		VisualViewport struct {
			PageX        float64 `json:"pageX"`
			PageY        float64 `json:"pageY"`
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"visualViewport"`
		CSSContentSize *struct {
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"cssContentSize"`
		ContentSize struct {
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"contentSize"`
	}
	if err := c.Send(ctx, sessID, "Page.getLayoutMetrics", nil, &metrics); err != nil {
		return nil, ScreenshotInfo{}, err
	}
	vp := metrics.VisualViewport
	if metrics.CSSVisualViewport != nil && metrics.CSSVisualViewport.ClientWidth > 0 {
		vp.PageX = metrics.CSSVisualViewport.PageX
		vp.PageY = metrics.CSSVisualViewport.PageY
		vp.ClientWidth = metrics.CSSVisualViewport.ClientWidth
		vp.ClientHeight = metrics.CSSVisualViewport.ClientHeight
	}
	cs := metrics.ContentSize
	if metrics.CSSContentSize != nil && metrics.CSSContentSize.Width > 0 {
		cs.Width = metrics.CSSContentSize.Width
		cs.Height = metrics.CSSContentSize.Height
	}

	x, y := 0, 0
	width := int(math.Round(vp.ClientWidth))
	height := int(math.Round(vp.ClientHeight))
	if width < 1 {
		width = 1280
	}
	if height < 1 {
		height = 720
	}
	captureBeyond := false
	params := map[string]any{
		"format":      "png",
		"fromSurface": true,
	}
	if options.FullPage {
		width = int(math.Ceil(cs.Width))
		height = int(math.Ceil(cs.Height))
		captureBeyond = true
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": width, "height": height, "scale": 1}
	} else if options.ClipW > 0 {
		x, y = options.ClipX, options.ClipY
		width, height = options.ClipW, options.ClipH
		captureBeyond = true
		params["clip"] = map[string]any{"x": x, "y": y, "width": width, "height": height, "scale": 1}
	}
	if width < 1 || height < 1 || width > 32768 || height > 32768 || int64(width)*int64(height) > 100_000_000 {
		return nil, ScreenshotInfo{}, errors.New("requested screenshot dimensions are outside supported bounds")
	}
	params["captureBeyondViewport"] = captureBeyond

	var capture struct {
		Data string `json:"data"`
	}
	if err := c.Send(ctx, sessID, "Page.captureScreenshot", params, &capture); err != nil {
		return nil, ScreenshotInfo{}, err
	}
	raw, err := decodeBase64(capture.Data)
	if err != nil {
		return nil, ScreenshotInfo{}, err
	}

	shotID := ""
	if !options.FullPage && options.ClipW == 0 {
		shotID = fmt.Sprintf("%s:p%x", epoch, time.Now().UnixNano())
		ts.mu.Lock()
		ts.viewport = &viewportCapture{
			ID:     shotID,
			Width:  width,
			Height: height,
			PageX:  vp.PageX,
			PageY:  vp.PageY,
		}
		ts.mu.Unlock()
	}

	return raw, ScreenshotInfo{
		TabID:        tabID,
		PageEpoch:    epoch,
		ScreenshotID: shotID,
		Title:        title,
		URL:          pageURL,
		X:            x,
		Y:            y,
		Width:        width,
		Height:       height,
		FullPage:     options.FullPage,
		MIMEType:     "image/png",
	}, nil
}

func (c *cdpClient) actTab(ctx context.Context, tabID, targetID string, action Action) (ActionResult, error) {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return ActionResult{}, err
	}

	// Build unified locator if semantic selector fields or ref were supplied.
	loc := action.Locator
	if loc.Ref == "" && action.Ref != "" {
		loc.Ref = action.Ref
	}

	var value any
	switch action.Kind {
	case "click", "double_click":
		pt, err := c.resolveActionPoint(ctx, ts, action, loc)
		if err != nil {
			return ActionResult{}, err
		}
		btn := action.Button
		if btn == "" {
			btn = "left"
		}
		clicks := 1
		if action.Kind == "double_click" {
			clicks = 2
		}
		if err := c.clickAt(ctx, ts, pt, btn, clicks); err != nil {
			return ActionResult{}, err
		}
		value = true

	case "hover":
		pt, err := c.resolveActionPoint(ctx, ts, action, loc)
		if err != nil {
			return ActionResult{}, err
		}
		btn := action.Button
		if btn == "" {
			btn = "none"
		}
		if err := c.dispatchMouse(ctx, ts, pt.SessionID, "mouseMoved", pt.X, pt.Y, btn, 0, 0, 0, 0); err != nil {
			return ActionResult{}, err
		}
		value = true

	case "drag":
		if action.ToX == nil || action.ToY == nil {
			return ActionResult{}, errors.New("drag requires to_x and to_y")
		}
		start, err := c.resolveActionPoint(ctx, ts, action, loc)
		if err != nil {
			return ActionResult{}, err
		}
		endX, endY := *action.ToX, *action.ToY
		btn := action.Button
		if btn == "" {
			btn = "left"
		}
		mask := mouseButtonMask(btn)
		_ = c.dispatchMouse(ctx, ts, start.SessionID, "mouseMoved", start.X, start.Y, "none", 0, 0, 0, 0)
		if err := c.dispatchMouse(ctx, ts, start.SessionID, "mousePressed", start.X, start.Y, btn, mask, 1, 0, 0); err != nil {
			return ActionResult{}, err
		}
		for step := 1; step <= 12; step++ {
			t := float64(step) / 12.0
			cx := start.X + (endX-start.X)*t
			cy := start.Y + (endY-start.Y)*t
			_ = c.dispatchMouse(ctx, ts, start.SessionID, "mouseMoved", cx, cy, btn, mask, 0, 0, 0)
		}
		_ = c.dispatchMouse(ctx, ts, start.SessionID, "mouseReleased", endX, endY, btn, 0, 1, 0, 0)
		value = true

	case "type_text", "set_value", "fill":
		pt, err := c.resolveActionPoint(ctx, ts, action, loc)
		if err != nil {
			return ActionResult{}, err
		}
		if err := c.clickAt(ctx, ts, pt, "left", 1); err != nil {
			return ActionResult{}, err
		}
		if action.Kind == "set_value" || action.Kind == "fill" {
			_ = c.dispatchKey(ctx, ts, pt.SessionID, "Control+A")
			_ = c.dispatchKey(ctx, ts, pt.SessionID, "Backspace")
		}
		if action.Text != "" {
			if err := c.sendAllowDialog(ctx, ts, pt.SessionID, "Input.insertText", map[string]any{"text": action.Text}, nil); err != nil {
				return ActionResult{}, err
			}
		}
		value = true

	case "press_key":
		sessID := ts.sessionID
		if !loc.empty() || action.ScreenshotID != "" {
			pt, err := c.resolveActionPoint(ctx, ts, action, loc)
			if err != nil {
				return ActionResult{}, err
			}
			if err := c.clickAt(ctx, ts, pt, "left", 1); err != nil {
				return ActionResult{}, err
			}
			sessID = pt.SessionID
		}
		if err := c.dispatchKey(ctx, ts, sessID, action.Key); err != nil {
			return ActionResult{}, err
		}
		value = true

	case "scroll":
		var pt resolvedPoint
		if !loc.empty() || action.ScreenshotID != "" || (action.X != nil && action.Y != nil) {
			var err error
			pt, err = c.resolveActionPoint(ctx, ts, action, loc)
			if err != nil {
				return ActionResult{}, err
			}
		} else {
			pt = resolvedPoint{SessionID: ts.sessionID, X: 400, Y: 300}
		}
		if err := c.dispatchMouse(ctx, ts, pt.SessionID, "mouseWheel", pt.X, pt.Y, "none", 0, 0, action.ScrollX, action.ScrollY); err != nil {
			return ActionResult{}, err
		}
		value = true

	case "select":
		target, err := c.resolveTarget(ctx, ts, loc)
		if err != nil {
			return ActionResult{}, err
		}
		fn := `function(option) {
			if (!(this instanceof HTMLSelectElement)) throw new Error('target is not a select element');
			const item = Array.from(this.options).find(e => e.value === option || e.text === option || e.label === option);
			if (!item) throw new Error('select option not found: ' + option);
			this.value = item.value;
			this.dispatchEvent(new Event('input', { bubbles: true }));
			this.dispatchEvent(new Event('change', { bubbles: true }));
			return item.value;
		}`
		value, err = c.callOnBackendNode(ctx, ts, target, fn, []any{action.Option})
		if err != nil {
			return ActionResult{}, err
		}

	case "check":
		if action.Checked == nil {
			return ActionResult{}, errors.New("check requires checked boolean")
		}
		target, err := c.resolveTarget(ctx, ts, loc)
		if err != nil {
			return ActionResult{}, err
		}
		fn := `function(checked) {
			if (!(this instanceof HTMLInputElement) || !['checkbox','radio'].includes(this.type)) throw new Error('target is not a checkbox or radio');
			if (this.checked !== checked) this.click();
			return this.checked;
		}`
		value, err = c.callOnBackendNode(ctx, ts, target, fn, []any{*action.Checked})
		if err != nil {
			return ActionResult{}, err
		}

	case "upload_files":
		target, err := c.resolveTarget(ctx, ts, loc)
		if err != nil {
			return ActionResult{}, err
		}
		if err := c.Send(ctx, target.SessionID, "DOM.setFileInputFiles", map[string]any{
			"files":         action.Files,
			"backendNodeId": target.BackendNodeID,
		}, nil); err != nil {
			return ActionResult{}, err
		}
		value = true

	case "handle_dialog":
		if action.Accept == nil {
			return ActionResult{}, errors.New("handle_dialog requires accept boolean")
		}
		if err := c.Send(ctx, ts.sessionID, "Page.handleJavaScriptDialog", map[string]any{
			"accept":     *action.Accept,
			"promptText": action.PromptText,
		}, nil); err != nil {
			return ActionResult{}, err
		}
		value = true

	case "wait_for":
		target, err := c.resolveTarget(ctx, ts, loc)
		if err != nil {
			return ActionResult{}, err
		}
		value = map[string]any{"found": true, "backend_node_id": target.BackendNodeID}

	case "evaluate":
		var resp struct {
			Result struct {
				Value any `json:"value"`
			} `json:"result"`
			ExceptionDetails *struct {
				Text      string `json:"text"`
				Exception *struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if err := c.sendAllowDialog(ctx, ts, ts.sessionID, "Runtime.evaluate", map[string]any{
			"expression":    action.Script,
			"awaitPromise":  true,
			"returnByValue": true,
			"userGesture":   true,
		}, &resp); err != nil {
			return ActionResult{}, err
		}
		if resp.ExceptionDetails != nil {
			msg := resp.ExceptionDetails.Text
			if resp.ExceptionDetails.Exception != nil && resp.ExceptionDetails.Exception.Description != "" {
				msg = resp.ExceptionDetails.Exception.Description
			}
			return ActionResult{}, errors.New(msg)
		}
		value = resp.Result.Value

	default:
		return ActionResult{}, fmt.Errorf("unsupported browser action %q", action.Kind)
	}

	return ActionResult{TabID: tabID, Kind: action.Kind, Success: true, Value: value}, nil
}

func (c *cdpClient) resolveActionPoint(ctx context.Context, ts *tabSession, action Action, loc Locator) (resolvedPoint, error) {
	if action.X != nil && action.Y != nil && loc.empty() {
		if action.ScreenshotID != "" {
			ts.mu.Lock()
			vp := ts.viewport
			ts.mu.Unlock()
			if vp == nil || vp.ID != action.ScreenshotID {
				return resolvedPoint{}, errors.New("screenshot_id is stale; capture viewport again")
			}
		}
		return resolvedPoint{SessionID: ts.sessionID, X: *action.X, Y: *action.Y}, nil
	}
	target, err := c.resolveTarget(ctx, ts, loc)
	if err != nil {
		return resolvedPoint{}, err
	}
	_ = c.Send(ctx, target.SessionID, "DOM.scrollIntoViewIfNeeded", map[string]any{
		"backendNodeId": target.BackendNodeID,
	}, nil)
	var box struct {
		Model struct {
			Content []float64 `json:"content"`
			Border  []float64 `json:"border"`
		} `json:"model"`
	}
	if err := c.Send(ctx, target.SessionID, "DOM.getBoxModel", map[string]any{
		"backendNodeId": target.BackendNodeID,
	}, &box); err != nil {
		return resolvedPoint{}, fmt.Errorf("element has no visible layout box: %w", err)
	}
	quad := box.Model.Content
	if len(quad) != 8 {
		quad = box.Model.Border
	}
	if len(quad) != 8 {
		return resolvedPoint{}, errors.New("target element has no visible box")
	}
	cx := (quad[0] + quad[2] + quad[4] + quad[6]) / 4.0
	cy := (quad[1] + quad[3] + quad[5] + quad[7]) / 4.0
	return resolvedPoint{SessionID: target.SessionID, X: math.Round(cx), Y: math.Round(cy)}, nil
}

// resolveTarget resolves either a snapshot ref or a live Playwright-style semantic/CSS locator
// with built-in auto-waiting up to TimeoutMS.
func (c *cdpClient) resolveTarget(ctx context.Context, ts *tabSession, loc Locator) (axTargetRef, error) {
	if loc.Ref != "" {
		ts.mu.Lock()
		marker := strings.LastIndex(loc.Ref, ":e")
		snapID := ""
		if marker > 0 {
			snapID = loc.Ref[:marker]
		}
		target, ok := ts.snapshots[snapID][loc.Ref]
		ts.mu.Unlock()
		if !ok {
			return axTargetRef{}, errors.New("element ref not found or stale; take a fresh snapshot or use a semantic locator")
		}
		return target, nil
	}
	if loc.empty() {
		return axTargetRef{}, errors.New("target ref or locator is required")
	}

	timeout := time.Duration(loc.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)

	var lastErr error
	for {
		target, err := c.tryResolveLocatorOnce(ctx, ts, loc)
		if err == nil {
			return target, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return axTargetRef{}, lastErr
		}
		select {
		case <-ctx.Done():
			return axTargetRef{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *cdpClient) tryResolveLocatorOnce(ctx context.Context, ts *tabSession, loc Locator) (axTargetRef, error) {
	// CSS / Text / Placeholder selectors resolved directly in DOM so they match
	// any element (even non-interactive containers or custom widgets).
	if loc.CSS != "" || loc.Text != "" || loc.Placeholder != "" {
		return c.resolveViaDOM(ctx, ts, loc)
	}
	// Role / Name / Label resolved directly against Accessibility.getFullAXTree
	// across the main frame and all OOPIF iframes.
	ts.mu.Lock()
	sessions := make([]string, 0, 1+len(ts.childSessions))
	sessions = append(sessions, ts.sessionID)
	for child := range ts.childSessions {
		sessions = append(sessions, child)
	}
	ts.mu.Unlock()

	wantRole := strings.ToLower(strings.TrimSpace(loc.Role))
	wantName := strings.TrimSpace(loc.Name)
	if wantName == "" {
		wantName = strings.TrimSpace(loc.Label)
	}
	var matches []axTargetRef
	for _, sessID := range sessions {
		var tree struct {
			Nodes []axNode `json:"nodes"`
		}
		if err := c.Send(ctx, sessID, "Accessibility.getFullAXTree", nil, &tree); err != nil {
			continue
		}
		for _, node := range tree.Nodes {
			if node.Ignored || node.BackendDOMNodeID == 0 {
				continue
			}
			role := strings.ToLower(axString(node.Role))
			if wantRole != "" && role != wantRole {
				continue
			}
			if loc.Label != "" && !interactiveAXRoles[role] {
				continue
			}
			name := strings.TrimSpace(axString(node.Name))
			if wantName != "" {
				if loc.Exact {
					if name != wantName {
						continue
					}
				} else if !strings.Contains(strings.ToLower(name), strings.ToLower(wantName)) {
					continue
				}
			}
			matches = append(matches, axTargetRef{SessionID: sessID, BackendNodeID: node.BackendDOMNodeID})
		}
	}
	idx := loc.Nth
	if idx < 0 {
		idx = 0
	}
	if len(matches) <= idx {
		return axTargetRef{}, fmt.Errorf("locator did not match any element (role=%q name=%q label=%q nth=%d)", loc.Role, wantName, loc.Label, loc.Nth)
	}
	return matches[idx], nil
}

func (c *cdpClient) resolveViaDOM(ctx context.Context, ts *tabSession, loc Locator) (axTargetRef, error) {
	locJSON, _ := json.Marshal(loc)
	expr := fmt.Sprintf(`(() => {
		const loc = %s;
		let list = [];
		if (loc.css) {
			list = Array.from(document.querySelectorAll(loc.css));
		} else if (loc.placeholder) {
			const needle = loc.placeholder.toLowerCase();
			list = Array.from(document.querySelectorAll('input[placeholder],textarea[placeholder]')).filter(el => {
				const ph = (el.getAttribute('placeholder') || '').trim();
				return loc.exact ? ph === loc.placeholder : ph.toLowerCase().includes(needle);
			});
		} else if (loc.text) {
			const needle = loc.text.toLowerCase();
			const all = Array.from(document.querySelectorAll('a,button,label,span,div,p,li,h1,h2,h3,h4,h5,h6,td,th,[role="button"],[role="link"],[role="tab"],[role="menuitem"]'));
			list = all.filter(el => {
				const txt = (el.innerText || el.textContent || '').trim();
				if (!txt) return false;
				const ok = loc.exact ? txt === loc.text : txt.toLowerCase().includes(needle);
				if (!ok) return false;
				// Prefer the innermost matching element.
				return !Array.from(el.children).some(ch => {
					const ct = (ch.innerText || ch.textContent || '').trim();
					return loc.exact ? ct === loc.text : ct.toLowerCase().includes(needle);
				});
			});
		}
		const idx = loc.nth > 0 ? loc.nth : 0;
		return list[idx] || null;
	})()`, string(locJSON))

	var evalRes struct {
		Result struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if err := c.Send(ctx, ts.sessionID, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": false,
	}, &evalRes); err != nil {
		return axTargetRef{}, err
	}
	if evalRes.Result.ObjectID == "" || evalRes.Result.Subtype == "null" {
		return axTargetRef{}, fmt.Errorf("DOM locator did not match any element (css=%q text=%q placeholder=%q)", loc.CSS, loc.Text, loc.Placeholder)
	}
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer relCancel()
		_ = c.Send(relCtx, ts.sessionID, "Runtime.releaseObject", map[string]any{"objectId": evalRes.Result.ObjectID}, nil)
	}()

	var desc struct {
		Node struct {
			BackendNodeID int `json:"backendNodeId"`
		} `json:"node"`
	}
	if err := c.Send(ctx, ts.sessionID, "DOM.describeNode", map[string]any{
		"objectId": evalRes.Result.ObjectID,
	}, &desc); err != nil {
		return axTargetRef{}, err
	}
	if desc.Node.BackendNodeID == 0 {
		return axTargetRef{}, errors.New("matched DOM node has no backendNodeId")
	}
	return axTargetRef{SessionID: ts.sessionID, BackendNodeID: desc.Node.BackendNodeID}, nil
}

func (c *cdpClient) callOnBackendNode(ctx context.Context, ts *tabSession, target axTargetRef, fn string, args []any) (any, error) {
	var resolved struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := c.Send(ctx, target.SessionID, "DOM.resolveNode", map[string]any{
		"backendNodeId": target.BackendNodeID,
	}, &resolved); err != nil {
		return nil, err
	}
	if resolved.Object.ObjectID == "" {
		return nil, errors.New("target element did not produce a remote object")
	}
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer relCancel()
		_ = c.Send(relCtx, target.SessionID, "Runtime.releaseObject", map[string]any{"objectId": resolved.Object.ObjectID}, nil)
	}()

	callArgs := make([]map[string]any, len(args))
	for i, a := range args {
		callArgs[i] = map[string]any{"value": a}
	}
	var resp struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := c.sendAllowDialog(ctx, ts, target.SessionID, "Runtime.callFunctionOn", map[string]any{
		"objectId":            resolved.Object.ObjectID,
		"functionDeclaration": fn,
		"arguments":           callArgs,
		"returnByValue":       true,
		"userGesture":         true,
	}, &resp); err != nil {
		return nil, err
	}
	if resp.ExceptionDetails != nil {
		msg := resp.ExceptionDetails.Text
		if resp.ExceptionDetails.Exception != nil && resp.ExceptionDetails.Exception.Description != "" {
			msg = resp.ExceptionDetails.Exception.Description
		}
		return nil, errors.New(msg)
	}
	return resp.Result.Value, nil
}

func (c *cdpClient) clickAt(ctx context.Context, ts *tabSession, pt resolvedPoint, button string, count int) error {
	if err := c.dispatchMouse(ctx, ts, pt.SessionID, "mouseMoved", pt.X, pt.Y, "none", 0, 0, 0, 0); err != nil {
		return err
	}
	mask := mouseButtonMask(button)
	if err := c.dispatchMouse(ctx, ts, pt.SessionID, "mousePressed", pt.X, pt.Y, button, mask, count, 0, 0); err != nil {
		return err
	}
	return c.dispatchMouse(ctx, ts, pt.SessionID, "mouseReleased", pt.X, pt.Y, button, 0, count, 0, 0)
}

func (c *cdpClient) dispatchMouse(ctx context.Context, ts *tabSession, sessionID, kind string, x, y float64, button string, buttons, clickCount, deltaX, deltaY int) error {
	params := map[string]any{
		"type":       kind,
		"x":          x,
		"y":          y,
		"button":     button,
		"buttons":    buttons,
		"clickCount": clickCount,
	}
	if kind == "mouseWheel" {
		params["deltaX"] = deltaX
		params["deltaY"] = deltaY
	}
	return c.sendAllowDialog(ctx, ts, sessionID, "Input.dispatchMouseEvent", params, nil)
}

func mouseButtonMask(button string) int {
	switch button {
	case "right":
		return 2
	case "middle":
		return 4
	default:
		return 1
	}
}

func (c *cdpClient) dispatchKey(ctx context.Context, ts *tabSession, sessionID, combination string) error {
	parts := strings.Split(combination, "+")
	var cleaned []string
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) == 0 {
		return errors.New("key is required")
	}
	keyName := cleaned[len(cleaned)-1]
	modifiers := 0
	for _, mod := range cleaned[:len(cleaned)-1] {
		switch strings.ToLower(mod) {
		case "alt":
			modifiers |= 1
		case "control", "ctrl":
			modifiers |= 2
		case "meta", "command", "cmd":
			modifiers |= 4
		case "shift":
			modifiers |= 8
		}
	}
	key, code, vk := lookupKeyInfo(keyName)
	payload := map[string]any{
		"key":                   key,
		"code":                  code,
		"windowsVirtualKeyCode": vk,
		"nativeVirtualKeyCode":  vk,
		"modifiers":             modifiers,
	}
	down := map[string]any{"type": "rawKeyDown"}
	up := map[string]any{"type": "keyUp"}
	for k, v := range payload {
		down[k] = v
		up[k] = v
	}
	if err := c.sendAllowDialog(ctx, ts, sessionID, "Input.dispatchKeyEvent", down, nil); err != nil {
		return err
	}
	return c.sendAllowDialog(ctx, ts, sessionID, "Input.dispatchKeyEvent", up, nil)
}

func lookupKeyInfo(value string) (key, code string, vk int) {
	named := map[string]struct {
		k, c string
		v    int
	}{
		"Enter":      {"Enter", "Enter", 13},
		"Tab":        {"Tab", "Tab", 9},
		"Escape":     {"Escape", "Escape", 27},
		"Esc":        {"Escape", "Escape", 27},
		"Backspace":  {"Backspace", "Backspace", 8},
		"Delete":     {"Delete", "Delete", 46},
		"Space":      {" ", "Space", 32},
		"ArrowLeft":  {"ArrowLeft", "ArrowLeft", 37},
		"Left":       {"ArrowLeft", "ArrowLeft", 37},
		"ArrowUp":    {"ArrowUp", "ArrowUp", 38},
		"Up":         {"ArrowUp", "ArrowUp", 38},
		"ArrowRight": {"ArrowRight", "ArrowRight", 39},
		"Right":      {"ArrowRight", "ArrowRight", 39},
		"ArrowDown":  {"ArrowDown", "ArrowDown", 40},
		"Down":       {"ArrowDown", "ArrowDown", 40},
		"Home":       {"Home", "Home", 36},
		"End":        {"End", "End", 35},
		"PageUp":     {"PageUp", "PageUp", 33},
		"PageDown":   {"PageDown", "PageDown", 34},
	}
	if info, ok := named[value]; ok {
		return info.k, info.c, info.v
	}
	if len(value) == 1 {
		ch := value[0]
		upper := strings.ToUpper(value)[0]
		if upper >= 'A' && upper <= 'Z' {
			return value, fmt.Sprintf("Key%c", upper), int(upper)
		}
		if ch >= '0' && ch <= '9' {
			return value, fmt.Sprintf("Digit%c", ch), int(ch)
		}
		return value, value, int(upper)
	}
	return value, value, 0
}
