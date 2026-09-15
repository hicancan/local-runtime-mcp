package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func browserCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("browser requires extension, token, status, tabs, open, close, navigate, snapshot, screenshot, or action")
	}
	if args[0] == "token" {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, hex.EncodeToString(value))
		return err
	}
	if args[0] == "extension" {
		flags := flagSet("browser extension", stderr)
		defaultDirectory, err := browser.DefaultExtensionDirectory()
		if err != nil {
			return err
		}
		directory := flags.String("directory", defaultDirectory, "extension output directory")
		configPath := flags.String("config", "", "configuration file whose bridge settings should be packaged")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		installed, err := browser.InstallExtension(*directory)
		if err != nil {
			return err
		}
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		configured := cfg.Browser.Enabled
		if configured {
			if err := browser.ConfigureExtension(installed, cfg.Browser.Listen, cfg.Browser.Token); err != nil {
				return err
			}
		}
		return writeJSON(stdout, struct {
			Directory  string `json:"directory"`
			Configured bool   `json:"configured"`
			NextStep   string `json:"next_step"`
		}{Directory: installed, Configured: configured, NextStep: "Open edge://extensions or chrome://extensions, enable Developer mode, choose Load unpacked, and select this directory."})
	}

	flags := flagSet("browser "+args[0], stderr)
	configPath := flags.String("config", "", "configuration file path")
	var (
		tabID       = flags.Int("tab", 0, "positive browser tab ID")
		url         = flags.String("url", "", "absolute URL")
		active      = flags.Bool("active", true, "make a new tab active")
		maxElements = flags.Int("max-elements", 500, "maximum interactive elements")
		maxText     = flags.Int("max-text", 50000, "maximum visible-text characters")
		kind        = flags.String("kind", "", "browser action kind")
		selector    = flags.String("selector", "", "CSS selector")
		ref         = flags.String("ref", "", "element reference from snapshot")
		text        = flags.String("text", "", "text to enter")
		key         = flags.String("key", "", "key or key combination")
		scrollX     = flags.Int("scroll-x", 0, "horizontal scroll amount")
		scrollY     = flags.Int("scroll-y", 0, "vertical scroll amount")
		script      = flags.String("script", "", "JavaScript expression")
	)
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, _, err := loadConfigRuntime(*configPath)
	if err != nil {
		return err
	}
	bridge, err := browser.Start(ctx, cfg.Browser)
	if err != nil {
		return err
	}
	defer bridge.Close(context.Background())
	if args[0] != "status" {
		if err := waitForBrowser(ctx, bridge, 5*time.Second); err != nil {
			return err
		}
	}
	switch args[0] {
	case "status":
		return writeJSON(stdout, bridge.Status())
	case "tabs":
		result, err := bridge.Tabs(ctx)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "open":
		result, err := bridge.Open(ctx, *url, *active)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "close":
		if err := bridge.CloseTab(ctx, *tabID); err != nil {
			return err
		}
		return writeJSON(stdout, map[string]bool{"success": true})
	case "navigate":
		result, err := bridge.Navigate(ctx, *tabID, *url)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "snapshot":
		result, err := bridge.Snapshot(ctx, *tabID, *maxElements, *maxText)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "screenshot":
		data, info, err := bridge.Screenshot(ctx, *tabID)
		if err != nil {
			return err
		}
		return writeJSON(stdout, struct {
			Info browser.ScreenshotInfo `json:"info"`
			Data string                 `json:"data_base64"`
		}{Info: info, Data: base64.StdEncoding.EncodeToString(data)})
	case "action":
		result, err := bridge.Act(ctx, browser.Action{Kind: *kind, TabID: *tabID, Selector: *selector, Ref: *ref, Text: *text, Key: *key, ScrollX: *scrollX, ScrollY: *scrollY, Script: *script})
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	default:
		return fmt.Errorf("unknown browser command %q", args[0])
	}
}

func waitForBrowser(ctx context.Context, bridge *browser.Bridge, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if status := bridge.Status(); !status.Enabled {
			return errors.New("browser bridge is disabled in the configuration")
		} else if status.Connected {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("browser extension did not connect within 5 seconds")
		case <-ticker.C:
		}
	}
}

func computerCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("computer requires screenshot or action")
	}
	controller := computer.New()
	switch args[0] {
	case "screenshot":
		flags := flagSet("computer screenshot", stderr)
		metadataOnly := flags.Bool("metadata-only", false, "omit base64 PNG data")
		outputPath := flags.String("output", "", "write PNG bytes to this path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		data, info, err := controller.Screenshot(ctx)
		if err != nil {
			return err
		}
		result := struct {
			Info computer.ScreenshotInfo `json:"info"`
			Data string                  `json:"data_base64,omitempty"`
			Path string                  `json:"output_path,omitempty"`
		}{Info: info}
		if *outputPath != "" {
			absolute, err := filepath.Abs(*outputPath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(absolute, data, 0o644); err != nil {
				return err
			}
			result.Path = absolute
		} else if !*metadataOnly {
			result.Data = base64.StdEncoding.EncodeToString(data)
		}
		return writeJSON(stdout, result)
	case "action":
		flags := flagSet("computer action", stderr)
		kind := flags.String("kind", "", "move, click, drag, type, key, or scroll")
		x := flags.Int("x", 0, "screen x coordinate")
		y := flags.Int("y", 0, "screen y coordinate")
		toX := flags.Int("to-x", 0, "drag destination x coordinate")
		toY := flags.Int("to-y", 0, "drag destination y coordinate")
		button := flags.String("button", "left", "left, middle, or right")
		text := flags.String("text", "", "Unicode text")
		key := flags.String("key", "", "key combination such as CTRL+L")
		scrollX := flags.Int("scroll-x", 0, "horizontal wheel delta")
		scrollY := flags.Int("scroll-y", 0, "vertical wheel delta")
		clickCount := flags.Int("click-count", 1, "click count from 1 to 3")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		result, err := controller.Act(ctx, computer.Action{Kind: *kind, X: *x, Y: *y, ToX: *toX, ToY: *toY, Button: *button, Text: *text, Key: *key, ScrollX: *scrollX, ScrollY: *scrollY, ClickCount: *clickCount})
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	default:
		return fmt.Errorf("unknown computer command %q", args[0])
	}
}
