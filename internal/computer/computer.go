package computer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Controller exposes one coherent view of the current interactive desktop.
// Coordinates are relative to the image returned by State: either the complete
// virtual desktop or one selected window.
type Controller interface {
	Backend() string
	Targets(context.Context) (TargetsResult, error)
	State(context.Context, StateOptions) ([]byte, State, error)
	Act(context.Context, Action) (ActionResult, error)
}

type Rectangle struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Window struct {
	ID     int64     `json:"id"`
	PID    int       `json:"pid,omitempty"`
	Title  string    `json:"title"`
	Bounds Rectangle `json:"bounds"`
	Active bool      `json:"active"`
}

type TargetsResult struct {
	Backend      string   `json:"backend"`
	Experimental bool     `json:"experimental"`
	Windows      []Window `json:"windows"`
}

type StateOptions struct {
	WindowID             int64 `json:"window_id,omitempty" jsonschema:"open window ID from computer_targets; omit for the complete virtual desktop"`
	IncludeAccessibility bool  `json:"include_accessibility,omitempty" jsonschema:"include visible native child controls when supported"`
}

type Element struct {
	Ref     string    `json:"ref"`
	Role    string    `json:"role"`
	Name    string    `json:"name,omitempty"`
	Bounds  Rectangle `json:"bounds"`
	Enabled bool      `json:"enabled"`
	Focused bool      `json:"focused,omitempty"`
}

type AccessibilityState struct {
	Elements  []Element `json:"elements"`
	Truncated bool      `json:"truncated"`
}

type State struct {
	Backend       string              `json:"backend"`
	StateID       string              `json:"state_id"`
	WindowID      int64               `json:"window_id,omitempty"`
	Title         string              `json:"title,omitempty"`
	OriginX       int                 `json:"origin_x"`
	OriginY       int                 `json:"origin_y"`
	Width         int                 `json:"width"`
	Height        int                 `json:"height"`
	CursorX       int                 `json:"cursor_x"`
	CursorY       int                 `json:"cursor_y"`
	MIMEType      string              `json:"mime_type"`
	Accessibility *AccessibilityState `json:"accessibility,omitempty"`
}

type Action struct {
	Kind       string `json:"kind" jsonschema:"desktop operation to perform"`
	WindowID   int64  `json:"window_id,omitempty" jsonschema:"target window ID; omit for the complete virtual desktop"`
	StateID    string `json:"state_id,omitempty" jsonschema:"state ID from computer_state; validates that bounds have not changed"`
	ElementRef string `json:"element_ref,omitempty" jsonschema:"native control reference from computer_state; requires state_id"`
	X          int    `json:"x,omitempty" jsonschema:"X coordinate relative to the computer_state image"`
	Y          int    `json:"y,omitempty" jsonschema:"Y coordinate relative to the computer_state image"`
	ToX        int    `json:"to_x,omitempty" jsonschema:"drag destination X coordinate relative to the state image"`
	ToY        int    `json:"to_y,omitempty" jsonschema:"drag destination Y coordinate relative to the state image"`
	Button     string `json:"button,omitempty" jsonschema:"mouse button; defaults to left"`
	Text       string `json:"text,omitempty" jsonschema:"Unicode text for type_text or set_value"`
	Key        string `json:"key,omitempty" jsonschema:"key or modifier combination such as CTRL+L"`
	ScrollX    int    `json:"scroll_x,omitempty" jsonschema:"horizontal wheel delta; positive scrolls right"`
	ScrollY    int    `json:"scroll_y,omitempty" jsonschema:"vertical wheel delta; positive scrolls down"`
}

type ActionResult struct {
	Backend  string `json:"backend"`
	Kind     string `json:"kind"`
	WindowID int64  `json:"window_id,omitempty"`
	Success  bool   `json:"success"`
}

func Validate(action Action) error {
	if action.WindowID < 0 {
		return errors.New("window_id cannot be negative")
	}
	if action.ElementRef != "" && action.StateID == "" {
		return errors.New("element_ref requires state_id")
	}
	switch action.Kind {
	case "activate", "move", "drag":
		return nil
	case "click", "double_click":
		if action.Button != "" && action.Button != "left" && action.Button != "middle" && action.Button != "right" {
			return errors.New("button must be left, middle, or right")
		}
		return nil
	case "type_text", "set_value":
		if action.Text == "" {
			return fmt.Errorf("text cannot be empty for %s", action.Kind)
		}
		return nil
	case "press_key":
		if strings.TrimSpace(action.Key) == "" {
			return errors.New("key cannot be empty")
		}
		return nil
	case "scroll":
		if action.ScrollX == 0 && action.ScrollY == 0 {
			return errors.New("scroll_x or scroll_y must be non-zero")
		}
		return nil
	default:
		return fmt.Errorf("unsupported computer action %q", action.Kind)
	}
}
