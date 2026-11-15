package computer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Controller exposes one coherent view of the current interactive desktop.
// Coordinates in State are relative to its origin: one window's visible frame, one display,
// or the complete virtual desktop; origins are physical pixels in the virtual desktop.
type Controller interface {
	Control(context.Context, ControlOptions) (ControlStatus, error)
	BeginDesktopOperation(context.Context, string) (context.Context, func(), error)
	Targets(context.Context, TargetsOptions) (TargetsResult, error)
	State(context.Context, StateOptions) ([]byte, State, error)
	Wait(context.Context, WaitOptions) ([]byte, State, error)
	Act(context.Context, Action) (ActionResult, error)
	Close() error
}

type Rectangle struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Display struct {
	DisplayID string    `json:"display_id"`
	Bounds    Rectangle `json:"bounds"`
	WorkArea  Rectangle `json:"work_area"`
	Scale     float64   `json:"scale"`
	Primary   bool      `json:"primary"`
}

type Window struct {
	TargetID  string    `json:"target_id"`
	PID       int       `json:"pid,omitempty"`
	Process   string    `json:"process,omitempty"`
	Title     string    `json:"title"`
	ClassName string    `json:"class_name,omitempty"`
	Bounds    Rectangle `json:"bounds"`
	State     string    `json:"state"`
	DisplayID string    `json:"display_id,omitempty"`
	Active    bool      `json:"active"`
	Tool      bool      `json:"tool,omitempty"`
	Topmost   bool      `json:"topmost,omitempty"`
}

type TargetsOptions struct {
	PID     int    `json:"pid,omitempty" jsonschema:"only windows of this process ID"`
	Title   string `json:"title,omitempty" jsonschema:"only windows whose title contains this text, case-insensitive"`
	Process string `json:"process,omitempty" jsonschema:"only windows whose executable name contains this text, case-insensitive"`
}

type TargetsResult struct {
	Displays []Display `json:"displays"`
	Windows  []Window  `json:"windows"`
}

type StateOptions struct {
	ControlID    string     `json:"control_id,omitempty" jsonschema:"control ID from computer_control acquire; only physical input needs it"`
	TargetID     string     `json:"target_id,omitempty" jsonschema:"opaque window target ID from computer_targets; UI Automation elements are read only for a window"`
	DisplayID    string     `json:"display_id,omitempty" jsonschema:"one display from computer_targets; omit both IDs for every display"`
	Region       *Rectangle `json:"region,omitempty" jsonschema:"crop of the observed area, relative to its origin"`
	Image        *bool      `json:"image,omitempty" jsonschema:"return pixels; defaults to true, false observes elements only"`
	Cursor       bool       `json:"cursor,omitempty" jsonschema:"draw the pointer into the image; defaults to false"`
	Elements     string     `json:"elements,omitempty" jsonschema:"all (default: named, identified or operable elements including text), interactive, or none"`
	Name         string     `json:"name,omitempty" jsonschema:"only elements whose name contains this text, case-insensitive"`
	AutomationID string     `json:"automation_id,omitempty" jsonschema:"only elements with exactly this AutomationId"`
	Role         string     `json:"role,omitempty" jsonschema:"only elements of this role, such as button, text, textbox, listitem"`
	MaxElements  int        `json:"max_elements,omitempty" jsonschema:"elements per page from 1 to 5000; defaults to 300"`
	Offset       int        `json:"offset,omitempty" jsonschema:"matching elements to skip, for the next page"`
}

type Element struct {
	RefID        string     `json:"ref_id"`
	Role         string     `json:"role"`
	Name         string     `json:"name,omitempty"`
	AutomationID string     `json:"automation_id,omitempty"`
	ClassName    string     `json:"class_name,omitempty"`
	Value        *string    `json:"value,omitempty"`
	Bounds       *Rectangle `json:"bounds,omitempty"`
	Depth        int        `json:"depth"`
	Parent       *int       `json:"parent,omitempty"`
	Disabled     bool       `json:"disabled,omitempty"`
	Offscreen    bool       `json:"offscreen,omitempty"`
	Focused      bool       `json:"focused,omitempty"`
	Patterns     []string   `json:"patterns,omitempty"`
}

type State struct {
	Actionable        bool      `json:"actionable"`
	UIAStatus         string    `json:"uia_status"`
	StateID           string    `json:"state_id"`
	TargetID          string    `json:"target_id,omitempty"`
	DisplayID         string    `json:"display_id,omitempty"`
	Title             string    `json:"title,omitempty"`
	OriginX           int       `json:"origin_x"`
	OriginY           int       `json:"origin_y"`
	Width             int       `json:"width"`
	Height            int       `json:"height"`
	Scale             float64   `json:"scale"`
	CursorX           int       `json:"cursor_x"`
	CursorY           int       `json:"cursor_y"`
	CursorShape       string    `json:"cursor_shape,omitempty" jsonschema:"the system cursor showing: arrow, move, hand, text, resize_*, wait, busy, not_allowed, crosshair; other for an app's own; hidden"`
	HasImage          bool      `json:"has_image"`
	MIMEType          string    `json:"mime_type"`
	Elements          []Element `json:"elements,omitempty"`
	ElementsTotal     int       `json:"elements_total"`
	ElementsTruncated bool      `json:"elements_truncated"`
	Satisfied         *bool     `json:"satisfied,omitempty"`
}

type WaitOptions struct {
	TargetID     string `json:"target_id" jsonschema:"opaque window target ID from computer_targets"`
	Name         string `json:"name,omitempty" jsonschema:"element name contains this text, case-insensitive"`
	AutomationID string `json:"automation_id,omitempty" jsonschema:"element AutomationId equals this"`
	Role         string `json:"role,omitempty" jsonschema:"element role equals this"`
	Until        string `json:"until,omitempty" jsonschema:"present (default) or absent"`
	TimeoutMS    int    `json:"timeout_ms,omitempty" jsonschema:"milliseconds from 100 to 60000; defaults to 5000"`
	Image        bool   `json:"image,omitempty" jsonschema:"return the window pixels with the final state"`
}

type Action struct {
	ControlID  string     `json:"control_id,omitempty" jsonschema:"control ID from computer_control acquire; only physical input needs it"`
	Kind       string     `json:"kind" jsonschema:"semantic (invoke, toggle, select, expand, collapse, set_value, scroll_into_view, scroll_by), window (place), or physical input"`
	TargetID   string     `json:"target_id,omitempty" jsonschema:"opaque target ID; required for place and activate"`
	StateID    string     `json:"state_id,omitempty" jsonschema:"state ID from computer_state; required for physical input"`
	ElementRef string     `json:"element_ref,omitempty" jsonschema:"element ref_id from computer_state or computer_wait"`
	X          *int       `json:"x,omitempty" jsonschema:"X coordinate relative to the computer_state image"`
	Y          *int       `json:"y,omitempty" jsonschema:"Y coordinate relative to the computer_state image"`
	ToX        *int       `json:"to_x,omitempty" jsonschema:"drag destination X coordinate relative to the state image"`
	ToY        *int       `json:"to_y,omitempty" jsonschema:"drag destination Y coordinate relative to the state image"`
	DurationMS int        `json:"duration_ms,omitempty" jsonschema:"how long a drag takes from press to release, as a person's hand would; default 200, at most 5000"`
	Button     string     `json:"button,omitempty" jsonschema:"mouse button; defaults to left"`
	Text       string     `json:"text,omitempty" jsonschema:"Unicode text for type_text or set_value"`
	Key        string     `json:"key,omitempty" jsonschema:"key or modifier combination such as CTRL+L"`
	ScrollX    int        `json:"scroll_x,omitempty" jsonschema:"horizontal amount: wheel delta for scroll, pages for scroll_by; positive scrolls right"`
	ScrollY    int        `json:"scroll_y,omitempty" jsonschema:"vertical amount: wheel delta for scroll, pages for scroll_by; positive scrolls down"`
	Bounds     *Rectangle `json:"bounds,omitempty" jsonschema:"place: the window's new visible frame in virtual-desktop pixels"`
	Show       string     `json:"show,omitempty" jsonschema:"place: restore, minimize or maximize; restore and minimize never activate the window"`
}

type ActionResult struct {
	Kind     string `json:"kind"`
	TargetID string `json:"target_id,omitempty"`
	Success  bool   `json:"success"`
}

// Semantic operates a control through its UI Automation pattern; place moves or shows a
// window. Neither touches the pointer, the keyboard or the foreground window, so neither
// needs desktop control.
func Semantic(kind string) bool {
	switch kind {
	case "invoke", "toggle", "select", "expand", "collapse", "set_value", "scroll_into_view", "scroll_by", "place":
		return true
	}
	return false
}

func Validate(action Action) error {
	if Semantic(action.Kind) {
		return validateSemantic(action)
	}
	if action.ControlID == "" {
		return errors.New("control_id is required; acquire computer_control first")
	}
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
		if action.DurationMS < 0 || action.DurationMS > 5000 {
			return errors.New("duration_ms must be between 0 and 5000")
		}
		return validateButton(action.Button)
	case "click", "double_click":
		if err := requireTarget(action, action.Kind); err != nil {
			return err
		}
		return validateButton(action.Button)
	case "type_text":
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

func validateSemantic(action Action) error {
	if action.X != nil || action.Y != nil || action.ToX != nil || action.ToY != nil {
		return fmt.Errorf("%s does not accept coordinates", action.Kind)
	}
	if action.Kind == "place" {
		if action.TargetID == "" {
			return errors.New("place requires target_id")
		}
		if action.Bounds == nil && action.Show == "" {
			return errors.New("place needs bounds, show, or both")
		}
		switch action.Show {
		case "", "restore", "minimize", "maximize":
		default:
			return errors.New("show must be restore, minimize or maximize")
		}
		return nil
	}
	if action.ElementRef == "" {
		return fmt.Errorf("%s requires element_ref", action.Kind)
	}
	if action.Kind == "scroll_by" && action.ScrollX == 0 && action.ScrollY == 0 {
		return errors.New("scroll_by needs scroll_x or scroll_y pages")
	}
	return nil
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
