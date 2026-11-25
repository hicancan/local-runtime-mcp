package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var interactiveAXRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true,
	"combobox": true, "listbox": true, "option": true, "checkbox": true,
	"radio": true, "switch": true, "slider": true, "spinbutton": true,
	"menuitem": true, "menuitemcheckbox": true, "menuitemradio": true,
	"tab": true, "treeitem": true,
}

// SnapshotOptions configures accessibility snapshot extraction and diffing.
type SnapshotOptions struct {
	MaxElements int
	MaxText     int
	Diff        bool // when true and a prior snapshot exists in the same navigation, emit compact ordered line diff
}

type axValue struct {
	Value any `json:"value"`
}

type axProperty struct {
	Name  string  `json:"name"`
	Value axValue `json:"value"`
}

type axNode struct {
	Ignored          bool         `json:"ignored"`
	Role             axValue      `json:"role"`
	Name             axValue      `json:"name"`
	Value            axValue      `json:"value"`
	BackendDOMNodeID int          `json:"backendDOMNodeId"`
	Properties       []axProperty `json:"properties"`
}

func axString(v axValue) string {
	if v.Value == nil {
		return ""
	}
	if s, ok := v.Value.(string); ok {
		return s
	}
	return fmt.Sprint(v.Value)
}

func (c *cdpClient) snapshotTab(ctx context.Context, tabID, targetID string, opts SnapshotOptions) (Snapshot, error) {
	ts, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return Snapshot{}, err
	}
	if opts.MaxElements <= 0 {
		opts.MaxElements = 500
	}
	if opts.MaxElements > 5000 {
		opts.MaxElements = 5000
	}
	if opts.MaxText <= 0 {
		opts.MaxText = 50000
	}
	if opts.MaxText > 500000 {
		opts.MaxText = 500000
	}

	title, pageURL := c.targetMeta(ctx, targetID)

	ts.mu.Lock()
	epoch := ts.epoch
	sessions := make([]string, 0, 1+len(ts.childSessions))
	sessions = append(sessions, ts.sessionID)
	for child := range ts.childSessions {
		sessions = append(sessions, child)
	}
	prevLines := append([]string(nil), ts.lastAXLines...)
	ts.snapshotSeq++
	snapNum := ts.snapshotSeq
	ts.mu.Unlock()

	// Fetch AX trees concurrently across main frame and OOPIF child frames while preserving frame order.
	type frameTree struct {
		sessID string
		nodes  []axNode
	}
	trees := make([]frameTree, len(sessions))
	if len(sessions) == 1 {
		var resp struct {
			Nodes []axNode `json:"nodes"`
		}
		if err := c.Send(ctx, sessions[0], "Accessibility.getFullAXTree", nil, &resp); err == nil {
			trees[0] = frameTree{sessID: sessions[0], nodes: resp.Nodes}
		}
	} else {
		var wg sync.WaitGroup
		for i, sid := range sessions {
			wg.Add(1)
			go func(idx int, sessID string) {
				defer wg.Done()
				var resp struct {
					Nodes []axNode `json:"nodes"`
				}
				if err := c.Send(ctx, sessID, "Accessibility.getFullAXTree", nil, &resp); err == nil {
					trees[idx] = frameTree{sessID: sessID, nodes: resp.Nodes}
				}
			}(i, sid)
		}
		wg.Wait()
	}

	snapshotID := fmt.Sprintf("%s:q%d", epoch, snapNum)
	elements := make([]SnapshotElement, 0, 64)
	refs := make(map[string]axTargetRef, 64)
	textLines := make([]string, 0, 128)
	elementsTruncated := false
	lastEmittedText := ""

	for _, ft := range trees {
		for _, node := range ft.nodes {
			if node.Ignored {
				continue
			}
			role := axString(node.Role)
			// Chromium always emits word/line-level InlineTextBox children duplicating StaticText.
			if role == "InlineTextBox" {
				continue
			}
			name := strings.TrimSpace(axString(node.Name))
			val := strings.TrimSpace(axString(node.Value))
			isInteractive := interactiveAXRoles[role]
			if name != "" {
				// Prune redundant StaticText child duplicating its immediately preceding parent label
				// (e.g. "[button] Submit" followed by StaticText "Submit" or heading followed by StaticText).
				if isInteractive {
					textLines = append(textLines, fmt.Sprintf("[%s] %s", role, name))
					lastEmittedText = name
				} else if role != "StaticText" || name != lastEmittedText {
					textLines = append(textLines, name)
					lastEmittedText = name
				}
			}
			if val != "" && val != name {
				textLines = append(textLines, val)
				lastEmittedText = val
			}
			if node.BackendDOMNodeID == 0 || !isInteractive {
				continue
			}
			if len(elements) >= opts.MaxElements {
				elementsTruncated = true
				continue
			}
			disabled := false
			for _, prop := range node.Properties {
				if prop.Name == "disabled" {
					if b, ok := prop.Value.Value.(bool); ok && b {
						disabled = true
					}
				}
			}
			ref := fmt.Sprintf("%s:e%d", snapshotID, len(elements)+1)
			refs[ref] = axTargetRef{SessionID: ft.sessID, BackendNodeID: node.BackendDOMNodeID}
			elements = append(elements, SnapshotElement{
				Ref:      ref,
				Role:     role,
				Name:     truncateRunes(name, 500),
				Value:    truncateRunes(val, 2000),
				Disabled: disabled,
			})
		}
	}

	ts.mu.Lock()
	if ts.epoch != epoch {
		ts.mu.Unlock()
		return Snapshot{}, errors.New("page changed during observation; request another snapshot")
	}
	ts.snapshots[snapshotID] = refs
	ts.snapshotOrder = append(ts.snapshotOrder, snapshotID)
	for len(ts.snapshotOrder) > 8 {
		expired := ts.snapshotOrder[0]
		ts.snapshotOrder = ts.snapshotOrder[1:]
		delete(ts.snapshots, expired)
	}
	ts.lastAXLines = textLines
	ts.mu.Unlock()

	var text string
	isDiff := false
	addedCount, removedCount := 0, 0
	if opts.Diff && len(prevLines) > 0 {
		diffText, added, removed := computeLineDiff(prevLines, textLines)
		text = diffText
		isDiff = true
		addedCount = added
		removedCount = removed
	} else {
		text = strings.Join(textLines, "\n")
	}
	textTruncated := false
	if len(text) > opts.MaxText {
		text = truncateUTF8Bytes(text, opts.MaxText)
		textTruncated = true
	}

	return Snapshot{
		TabID:             tabID,
		PageEpoch:         epoch,
		SnapshotID:        snapshotID,
		Title:             title,
		URL:               pageURL,
		Text:              text,
		TextTruncated:     textTruncated,
		Diff:              isDiff,
		AddedLines:        addedCount,
		RemovedLines:      removedCount,
		Elements:          elements,
		ElementsTruncated: elementsTruncated,
	}, nil
}

// computeLineDiff produces an order-aware Myers O(ND) shortest edit script diff
// between previous and current AX snapshots (with common prefix/suffix trimming
// and an O(N) fallback when edit distance exceeds 512 lines).
func computeLineDiff(prev, curr []string) (string, int, int) {
	prefix := 0
	for prefix < len(prev) && prefix < len(curr) && prev[prefix] == curr[prefix] {
		prefix++
	}
	pEnd := len(prev)
	cEnd := len(curr)
	for pEnd > prefix && cEnd > prefix && prev[pEnd-1] == curr[cEnd-1] {
		pEnd--
		cEnd--
	}
	a := prev[prefix:pEnd]
	b := curr[prefix:cEnd]
	if len(a) == 0 && len(b) == 0 {
		return fmt.Sprintf("(no AX text changes; %d lines unchanged)", len(curr)), 0, 0
	}

	out, added, removed, ok := myersLineDiff(a, b, 512)
	if !ok {
		out, added, removed = bagLineDiff(a, b)
	}
	if added == 0 && removed == 0 {
		return fmt.Sprintf("(no AX text changes; %d lines unchanged)", len(curr)), 0, 0
	}
	header := fmt.Sprintf("[AX diff: +%d / -%d lines, total %d]", added, removed, len(curr))
	return header + "\n" + strings.Join(out, "\n"), added, removed
}

func myersLineDiff(a, b []string, maxD int) ([]string, int, int, bool) {
	n, m := len(a), len(b)
	limit := n + m
	if limit > maxD {
		limit = maxD
	}
	offset := limit
	v := make([]int, 2*limit+1)
	trace := make([][]int, 0, min(limit+1, 64))

	foundD := -1
	for d := 0; d <= limit; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1+offset] < v[k+1+offset]) {
				x = v[k+1+offset]
			} else {
				x = v[k-1+offset] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[k+offset] = x
			if x >= n && y >= m {
				foundD = d
				break
			}
		}
		if foundD >= 0 {
			break
		}
	}
	if foundD < 0 {
		return nil, 0, 0, false
	}

	// Backtrack shortest edit script in reverse, emitting only inserts & deletes in exact order.
	rev := make([]string, 0, foundD)
	x, y := n, m
	added, removed := 0, 0
	for d := foundD; d > 0; d-- {
		prevV := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && prevV[k-1+offset] < prevV[k+1+offset]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := prevV[prevK+offset]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
		}
		if x == prevX {
			rev = append(rev, "+ "+b[prevY])
			added++
		} else {
			rev = append(rev, "- "+a[prevX])
			removed++
		}
		x, y = prevX, prevY
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, added, removed, true
}

func bagLineDiff(prev, curr []string) ([]string, int, int) {
	prevCounts := make(map[string]int, len(prev))
	for _, l := range prev {
		prevCounts[l]++
	}
	currCounts := make(map[string]int, len(curr))
	for _, l := range curr {
		currCounts[l]++
	}
	var out []string
	removed := 0
	remSeen := make(map[string]int, len(prev))
	for _, l := range prev {
		remSeen[l]++
		if remSeen[l] > currCounts[l] {
			out = append(out, "- "+l)
			removed++
		}
	}
	added := 0
	addSeen := make(map[string]int, len(curr))
	for _, l := range curr {
		addSeen[l]++
		if addSeen[l] > prevCounts[l] {
			out = append(out, "+ "+l)
			added++
		}
	}
	return out, added, removed
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) == 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
