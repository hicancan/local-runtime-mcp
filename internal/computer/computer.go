package computer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type Controller interface {
	Backend() string
	Screenshot(context.Context) ([]byte, ScreenshotInfo, error)
	Act(context.Context, Action) (ActionResult, error)
}

type ScreenshotInfo struct {
	Backend  string `json:"backend"`
	OriginX  int    `json:"origin_x"`
	OriginY  int    `json:"origin_y"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	CursorX  int    `json:"cursor_x"`
	CursorY  int    `json:"cursor_y"`
	MIMEType string `json:"mime_type"`
}

type Action struct {
	Kind       string `json:"kind"`
	X          int    `json:"x,omitempty"`
	Y          int    `json:"y,omitempty"`
	ToX        int    `json:"to_x,omitempty"`
	ToY        int    `json:"to_y,omitempty"`
	Button     string `json:"button,omitempty"`
	Text       string `json:"text,omitempty"`
	Key        string `json:"key,omitempty"`
	ScrollX    int    `json:"scroll_x,omitempty"`
	ScrollY    int    `json:"scroll_y,omitempty"`
	ClickCount int    `json:"click_count,omitempty"`
}

type ActionResult struct {
	Backend string `json:"backend"`
	Kind    string `json:"kind"`
	Success bool   `json:"success"`
}

func Validate(action Action) error {
	switch action.Kind {
	case "move", "drag":
		return nil
	case "click":
		if action.Button != "" && action.Button != "left" && action.Button != "middle" && action.Button != "right" {
			return errors.New("button must be left, middle, or right")
		}
		if action.ClickCount < 0 || action.ClickCount > 3 {
			return errors.New("click_count must be between 1 and 3")
		}
		return nil
	case "type":
		if action.Text == "" {
			return errors.New("text cannot be empty for type")
		}
		return nil
	case "key":
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
