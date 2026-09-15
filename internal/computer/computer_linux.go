//go:build linux

package computer

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type commandController struct{}

func New() Controller                      { return &commandController{} }
func (*commandController) Backend() string { return "linux-desktop" }

func (c *commandController) Targets(context.Context) (TargetsResult, error) {
	return TargetsResult{Backend: c.Backend(), Experimental: true, Windows: []Window{}}, nil
}

func (c *commandController) State(ctx context.Context, options StateOptions) ([]byte, State, error) {
	if options.WindowID != 0 {
		return nil, State{}, errors.New("window targeting is not implemented by the Linux command backend")
	}
	file, err := os.CreateTemp("", "lrmcp-screen-*.png")
	if err != nil {
		return nil, State{}, err
	}
	path := file.Name()
	_ = file.Close()
	defer os.Remove(path)
	commands := [][]string{{"gnome-screenshot", "-f", path}, {"scrot", path}}
	var lastError error
	for _, command := range commands {
		if output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput(); err != nil {
			lastError = fmt.Errorf("%s: %w (%s)", command[0], err, strings.TrimSpace(string(output)))
			continue
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
	return nil, State{}, fmt.Errorf("install gnome-screenshot or scrot: %w", lastError)
}

func (c *commandController) Act(ctx context.Context, action Action) (ActionResult, error) {
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	if action.WindowID != 0 || action.StateID != "" || action.ElementRef != "" {
		return ActionResult{}, errors.New("window and state targeting are not implemented by the Linux command backend")
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		return ActionResult{}, errors.New("xdotool is required for Linux desktop input")
	}
	var args []string
	switch action.Kind {
	case "move":
		args = []string{"mousemove", strconv.Itoa(action.X), strconv.Itoa(action.Y)}
	case "click", "double_click":
		button := "1"
		if action.Button == "middle" {
			button = "2"
		}
		if action.Button == "right" {
			button = "3"
		}
		count := 1
		if action.Kind == "double_click" {
			count = 2
		}
		args = []string{"mousemove", strconv.Itoa(action.X), strconv.Itoa(action.Y), "click", "--repeat", strconv.Itoa(count), button}
	case "drag":
		args = []string{"mousemove", strconv.Itoa(action.X), strconv.Itoa(action.Y), "mousedown", "1", "mousemove", "--sync", strconv.Itoa(action.ToX), strconv.Itoa(action.ToY), "mouseup", "1"}
	case "type_text":
		args = []string{"type", "--clearmodifiers", "--", action.Text}
	case "set_value":
		args = []string{"key", "--clearmodifiers", "ctrl+a", "type", "--clearmodifiers", "--", action.Text}
	case "press_key":
		args = []string{"key", "--clearmodifiers", action.Key}
	case "activate":
		return ActionResult{}, errors.New("window targeting is not implemented by the Linux command backend")
	case "scroll":
		appendScroll := func(amount int, negativeButton, positiveButton string) {
			if amount == 0 {
				return
			}
			button := positiveButton
			if amount < 0 {
				button, amount = negativeButton, -amount
			}
			args = append(args, "click", "--repeat", strconv.Itoa(max(1, amount/120)), button)
		}
		appendScroll(action.ScrollY, "4", "5")
		appendScroll(action.ScrollX, "6", "7")
	}
	if output, err := exec.CommandContext(ctx, "xdotool", args...).CombinedOutput(); err != nil {
		return ActionResult{}, fmt.Errorf("xdotool: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return ActionResult{Backend: c.Backend(), Kind: action.Kind, Success: true}, nil
}
