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
	Targets(context.Context) (TargetsResult, error)
	State(context.Context, StateOptions) ([]byte, State, error)
	Act(context.Context, Action) (ActionResult, error)
	Close() error
}

type Rectangle struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Window struct {
	TargetID string    `json:"target_id"`
	PID      int       `json:"pid,omitempty"`
	Title    string    `json:"title"`
	Bounds   Rectangle `json:"bounds"`
	Active   bool      `json:"active"`
}

type TargetsResult struct {
	Windows []Window `json:"windows"`
}

type StateOptions struct {
	TargetID string `json:"target_id,omitempty" jsonschema:"opaque target ID from computer_targets; omit for the complete virtual desktop"`
}

type Element struct {
	RefID    string    `json:"ref_id"`
	Role     string    `json:"role"`
	Name     string    `json:"name,omitempty"`
	Bounds   Rectangle `json:"bounds"`
	Disabled bool      `json:"disabled,omitempty"`
}

type State struct {
	StateID           string    `json:"state_id"`
	TargetID          string    `json:"target_id,omitempty"`
	Title             string    `json:"title,omitempty"`
	OriginX           int       `json:"origin_x"`
	OriginY           int       `json:"origin_y"`
	Width             int       `json:"width"`
	Height            int       `json:"height"`
	CursorX           int       `json:"cursor_x"`
	CursorY           int       `json:"cursor_y"`
	MIMEType          string    `json:"mime_type"`
	Elements          []Element `json:"elements,omitempty"`
	ElementsTruncated bool      `json:"elements_truncated"`
}

type Action struct {
	Kind       string `json:"kind" jsonschema:"desktop operation to perform"`
	TargetID   string `json:"target_id,omitempty" jsonschema:"opaque target ID; omit for the complete virtual desktop"`
	StateID    string `json:"state_id,omitempty" jsonschema:"state ID from computer_state; required for every action except activate"`
	ElementRef string `json:"element_ref,omitempty" jsonschema:"UI Automation element reference from the exact computer_state"`
	X          *int   `json:"x,omitempty" jsonschema:"X coordinate relative to the computer_state image"`
	Y          *int   `json:"y,omitempty" jsonschema:"Y coordinate relative to the computer_state image"`
	ToX        *int   `json:"to_x,omitempty" jsonschema:"drag destination X coordinate relative to the state image"`
	ToY        *int   `json:"to_y,omitempty" jsonschema:"drag destination Y coordinate relative to the state image"`
	Button     string `json:"button,omitempty" jsonschema:"mouse button; defaults to left"`
	Text       string `json:"text,omitempty" jsonschema:"Unicode text for type_text or set_value"`
	Key        string `json:"key,omitempty" jsonschema:"key or modifier combination such as CTRL+L"`
	ScrollX    int    `json:"scroll_x,omitempty" jsonschema:"horizontal wheel delta; positive scrolls right"`
	ScrollY    int    `json:"scroll_y,omitempty" jsonschema:"vertical wheel delta; positive scrolls down"`
}

type ActionResult struct {
	Kind     string `json:"kind"`
	TargetID string `json:"target_id,omitempty"`
	Success  bool   `json:"success"`
}

func Validate(action Action) error {
	if action.Kind == "activate" {
		if action.TargetID == "" {
			return errors.New("activate requires target_id")
		}
		if action.StateID != "" {
			return errors.New("activate does not accept state_id")
		}
		return nil
	}
	if action.StateID == "" {
		return errors.New("state_id is required; call computer_state immediately before acting")
	}
	switch action.Kind {
	case "move":
		return requireTarget(action, "move")
	case "drag":
		if err := requireTarget(action, "drag"); err != nil {
			return err
		}
		if err := requirePoint(action.ToX, action.ToY, "drag destination"); err != nil {
			return err
		}
		return validateButton(action.Button)
	case "click", "double_click":
		if err := requireTarget(action, action.Kind); err != nil {
			return err
		}
		return validateButton(action.Button)
	case "type_text", "set_value":
		if action.Text == "" {
			return fmt.Errorf("text cannot be empty for %s", action.Kind)
		}
		return validateOptionalTarget(action, action.Kind)
	case "press_key":
		if strings.TrimSpace(action.Key) == "" {
			return errors.New("key cannot be empty")
		}
		return validateOptionalTarget(action, "press_key")
	case "scroll":
		if action.ScrollX == 0 && action.ScrollY == 0 {
			return errors.New("scroll_x or scroll_y must be non-zero")
		}
		if (action.X == nil) != (action.Y == nil) {
			return errors.New("scroll x and y must be provided together")
		}
		if action.ElementRef != "" && action.X != nil {
			return errors.New("scroll accepts element_ref or coordinates, not both")
		}
		return nil
	default:
		return fmt.Errorf("unsupported computer action %q", action.Kind)
	}
}

func requireTarget(action Action, operation string) error {
	if action.ElementRef != "" {
		if action.X != nil || action.Y != nil {
			return fmt.Errorf("%s accepts element_ref or coordinates, not both", operation)
		}
		return nil
	}
	return requirePoint(action.X, action.Y, operation)
}

func validateOptionalTarget(action Action, operation string) error {
	if (action.X == nil) != (action.Y == nil) {
		return fmt.Errorf("%s x and y must be provided together", operation)
	}
	if action.ElementRef != "" && action.X != nil {
		return fmt.Errorf("%s accepts element_ref or coordinates, not both", operation)
	}
	return nil
}

func requirePoint(x, y *int, operation string) error {
	if x == nil || y == nil {
		return fmt.Errorf("%s requires x and y", operation)
	}
	return nil
}

func validateButton(button string) error {
	if button != "" && button != "left" && button != "middle" && button != "right" {
		return errors.New("button must be left, middle, or right")
	}
	return nil
}
