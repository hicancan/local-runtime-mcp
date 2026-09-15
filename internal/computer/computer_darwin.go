//go:build darwin

package computer

import (
	"context"
	"errors"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"strconv"
	"time"
)

type commandController struct{}

func New() Controller                      { return &commandController{} }
func (*commandController) Backend() string { return "macos-cliclick" }

func (c *commandController) Targets(context.Context) (TargetsResult, error) {
	return TargetsResult{Backend: c.Backend(), Experimental: true, Windows: []Window{}}, nil
}

func (c *commandController) State(ctx context.Context, options StateOptions) ([]byte, State, error) {
	if options.WindowID != 0 {
		return nil, State{}, errors.New("window targeting is not implemented by the macOS command backend")
	}
	file, err := os.CreateTemp("", "lrmcp-screen-*.png")
	if err != nil {
		return nil, State{}, err
	}
	path := file.Name()
	_ = file.Close()
	defer os.Remove(path)
	if output, err := exec.CommandContext(ctx, "screencapture", "-x", "-t", "png", path).CombinedOutput(); err != nil {
		return nil, State{}, errors.New(string(output))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, State{}, err
	}
	configuration, _, err := image.DecodeConfig(bytesReader(data))
	if err != nil {
		return nil, State{}, err
	}
	return data, State{Backend: c.Backend(), StateID: strconv.FormatInt(time.Now().UnixNano(), 10), Width: configuration.Width, Height: configuration.Height, MIMEType: "image/png"}, nil
}

func (c *commandController) Act(ctx context.Context, action Action) (ActionResult, error) {
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	if action.WindowID != 0 || action.StateID != "" || action.ElementRef != "" {
		return ActionResult{}, errors.New("window and state targeting are not implemented by the macOS command backend")
	}
	var arguments []string
	switch action.Kind {
	case "move":
		arguments = []string{"m:" + coordinates(action.X, action.Y)}
	case "click", "double_click":
		prefix := "c:"
		if action.Kind == "double_click" {
			prefix = "dc:"
		}
		if action.Button == "right" {
			prefix = "rc:"
		}
		arguments = []string{prefix + coordinates(action.X, action.Y)}
	case "drag":
		arguments = []string{"dd:" + coordinates(action.X, action.Y), "du:" + coordinates(action.ToX, action.ToY)}
	case "type_text":
		arguments = []string{"t:" + action.Text}
	case "set_value":
		arguments = []string{"kp:cmd+a", "t:" + action.Text}
	case "press_key":
		arguments = []string{"kp:" + action.Key}
	case "activate":
		return ActionResult{}, errors.New("window targeting is not implemented by the macOS command backend")
	case "scroll":
		if action.ScrollX != 0 {
			return ActionResult{}, errors.New("horizontal scrolling is not implemented by the macOS command backend")
		}
		arguments = []string{"w:" + strconv.Itoa(-action.ScrollY)}
	}
	if output, err := exec.CommandContext(ctx, "cliclick", arguments...).CombinedOutput(); err != nil {
		return ActionResult{}, errors.New("cliclick is required for macOS input: " + string(output))
	}
	return ActionResult{Backend: c.Backend(), Kind: action.Kind, Success: true}, nil
}

func coordinates(x, y int) string { return strconv.Itoa(x) + "," + strconv.Itoa(y) }
