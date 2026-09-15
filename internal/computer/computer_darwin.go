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
)

type commandController struct{}

func New() Controller                      { return &commandController{} }
func (*commandController) Backend() string { return "macos-cliclick" }

func (c *commandController) Screenshot(ctx context.Context) ([]byte, ScreenshotInfo, error) {
	file, err := os.CreateTemp("", "lrmcp-screen-*.png")
	if err != nil {
		return nil, ScreenshotInfo{}, err
	}
	path := file.Name()
	_ = file.Close()
	defer os.Remove(path)
	if output, err := exec.CommandContext(ctx, "screencapture", "-x", "-t", "png", path).CombinedOutput(); err != nil {
		return nil, ScreenshotInfo{}, errors.New(string(output))
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

func (c *commandController) Act(ctx context.Context, action Action) (ActionResult, error) {
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	var arguments []string
	switch action.Kind {
	case "move":
		arguments = []string{"m:" + coordinates(action.X, action.Y)}
	case "click":
		prefix := "c:"
		if action.ClickCount > 1 {
			prefix = "dc:"
		}
		if action.Button == "right" {
			prefix = "rc:"
		}
		arguments = []string{prefix + coordinates(action.X, action.Y)}
	case "drag":
		arguments = []string{"dd:" + coordinates(action.X, action.Y), "du:" + coordinates(action.ToX, action.ToY)}
	case "type":
		arguments = []string{"t:" + action.Text}
	case "key":
		arguments = []string{"kp:" + action.Key}
	case "scroll":
		arguments = []string{"w:" + strconv.Itoa(action.ScrollY)}
	}
	if output, err := exec.CommandContext(ctx, "cliclick", arguments...).CombinedOutput(); err != nil {
		return ActionResult{}, errors.New("cliclick is required for macOS input: " + string(output))
	}
	return ActionResult{Backend: c.Backend(), Kind: action.Kind, Success: true}, nil
}

func coordinates(x, y int) string { return strconv.Itoa(x) + "," + strconv.Itoa(y) }
