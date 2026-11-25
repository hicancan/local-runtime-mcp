package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	Diff        bool // when true and a prior snapshot exists in the same navigation, emit compact line diff
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
	snapNum := len(ts.snapshotOrder) + 1
	ts.mu.Unlock()

	snapshotID := fmt.Sprintf("%s:q%d", epoch, snapNum)
	elements := make([]SnapshotElement, 0, 64)
	refs := make(map[string]axTargetRef, 64)
	textLines := make([]string, 0, 128)
	elementsTruncated := false

	for _, sessID := range sessions {
		var tree struct {
			Nodes []axNode `json:"nodes"`
		}
		if err := c.Send(ctx, sessID, "Accessibility.getFullAXTree", nil, &tree); err != nil {
			continue
		}
		for _, node := range tree.Nodes {
			if node.Ignored {
				continue
			}
			role := axString(node.Role)
			name := strings.TrimSpace(axString(node.Name))
			val := strings.TrimSpace(axString(node.Value))
			if name != "" {
				if interactiveAXRoles[role] {
					textLines = append(textLines, fmt.Sprintf("[%s] %s", role, name))
				} else {
					textLines = append(textLines, name)
				}
			}
			if val != "" && val != name {
				textLines = append(textLines, val)
			}
			if node.BackendDOMNodeID == 0 || !interactiveAXRoles[role] {
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
			refs[ref] = axTargetRef{SessionID: sessID, BackendNodeID: node.BackendDOMNodeID}
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
		text = text[:opts.MaxText]
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

// computeLineDiff produces a compact Codex-style line diff between previous and
// current AX snapshots without pulling in heavy external diff dependencies.
func computeLineDiff(prev, curr []string) (string, int, int) {
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
	if added == 0 && removed == 0 {
		return fmt.Sprintf("(no AX text changes; %d lines unchanged)", len(curr)), 0, 0
	}
	header := fmt.Sprintf("[AX diff: +%d / -%d lines, total %d]", added, removed, len(curr))
	return header + "\n" + strings.Join(out, "\n"), added, removed
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
