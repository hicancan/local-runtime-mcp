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
)

type commandController struct{}

func New() Controller                      { return &commandController{} }
func (*commandController) Backend() string { return "linux-desktop" }

func (c *commandController) Screenshot(ctx context.Context) ([]byte, ScreenshotInfo, error) {
	file, err := os.CreateTemp("", "lrmcp-screen-*.png")
	if err != nil {
		return nil, ScreenshotInfo{}, err
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
			return nil, ScreenshotInfo{}, err
		}
		configuration, _, err := image.DecodeConfig(bytesReader(data))
		if err != nil {
			return nil, ScreenshotInfo{}, err
		}
		return data, ScreenshotInfo{Backend: c.Backend(), Width: configuration.Width, Height: configuration.Height, MIMEType: "image/png"}, nil
	}
	return nil, ScreenshotInfo{}, fmt.Errorf("install gnome-screenshot or scrot: %w", lastError)
}

func (c *commandController) Act(ctx context.Context, action Action) (ActionResult, error) {
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		return ActionResult{}, errors.New("xdotool is required for Linux desktop input")
	}
	var args []string
	switch action.Kind {
	case "move":
		args = []string{"mousemove", strconv.Itoa(action.X), strconv.Itoa(action.Y)}
	case "click":
		button := "1"
		if action.Button == "middle" {
			button = "2"
		}
		if action.Button == "right" {
			button = "3"
		}
		count := action.ClickCount
		if count == 0 {
			count = 1
		}
		args = []string{"mousemove", strconv.Itoa(action.X), strconv.Itoa(action.Y), "click", "--repeat", strconv.Itoa(count), button}
	case "drag":
		args = []string{"mousemove", strconv.Itoa(action.X), strconv.Itoa(action.Y), "mousedown", "1", "mousemove", "--sync", strconv.Itoa(action.ToX), strconv.Itoa(action.ToY), "mouseup", "1"}
	case "type":
		args = []string{"type", "--clearmodifiers", "--", action.Text}
	case "key":
		args = []string{"key", "--clearmodifiers", action.Key}
	case "scroll":
		button := "4"
		amount := action.ScrollY
		if amount < 0 {
			button, amount = "5", -amount
		}
		args = []string{"click", "--repeat", strconv.Itoa(max(1, amount/120)), button}
	}
	if output, err := exec.CommandContext(ctx, "xdotool", args...).CombinedOutput(); err != nil {
		return ActionResult{}, fmt.Errorf("xdotool: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return ActionResult{Backend: c.Backend(), Kind: action.Kind, Success: true}, nil
}
