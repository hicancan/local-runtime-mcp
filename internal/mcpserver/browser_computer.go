package mcpserver

import (
	"context"
	"errors"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerBrowserTools(server *mcp.Server, bridge *browser.Bridge) {
	mcp.AddTool(server, tool("browser_status", "Get browser bridge status", "Check whether the bundled Chromium extension is connected to the authenticated loopback bridge.", true, false, true, false), browserStatus(bridge))
	mcp.AddTool(server, tool("browser_tabs", "List browser tabs", "List controllable tabs in the Chromium profile running the bundled extension.", true, false, true, false), browserTabs(bridge))
	mcp.AddTool(server, tool("browser_open", "Open browser tab", "Open a URL in a new browser tab.", false, false, false, true), browserOpen(bridge))
	mcp.AddTool(server, tool("browser_close", "Close browser tab", "Close a browser tab by its numeric ID.", false, true, true, false), browserClose(bridge))
	mcp.AddTool(server, tool("browser_navigate", "Navigate browser tab", "Navigate an existing browser tab to a URL.", false, false, false, true), browserNavigate(bridge))
	mcp.AddTool(server, tool("browser_snapshot", "Read browser page", "Return visible page text and referenced interactive elements for reliable browser actions.", true, false, true, true), browserSnapshot(bridge))
	mcp.AddTool(server, tool("browser_screenshot", "Capture browser viewport", "Return the current tab viewport as native PNG image content.", true, false, true, true), browserScreenshot(bridge))
	mcp.AddTool(server, tool("browser_action", "Act on browser page", "Click or type into a referenced element, press a key, scroll, evaluate JavaScript, or navigate browser history.", false, true, false, true), browserAction(bridge))
}

func registerComputerTools(server *mcp.Server, controller computer.Controller) {
	mcp.AddTool(server, tool("computer_screenshot", "Capture desktop", "Return the current desktop as native PNG image content with virtual-screen coordinates.", true, false, true, false), computerScreenshot(controller))
	mcp.AddTool(server, tool("computer_action", "Control desktop", "Move, click, or drag the pointer; type Unicode text; press a key combination; or scroll the desktop.", false, true, false, false), computerAction(controller))
}

func requireBridge(bridge *browser.Bridge) error {
	if bridge == nil {
		return errors.New("browser bridge is unavailable")
	}
	return nil
}

func browserStatus(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, browser.Status, error) {
	return func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, browser.Status, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, browser.Status{}, err
		}
		return nil, bridge.Status(), nil
	}
}

type browserTabsOutput struct {
	Tabs []browser.Tab `json:"tabs"`
}

func browserTabs(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, browserTabsOutput, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, browserTabsOutput, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, browserTabsOutput{}, err
		}
		tabs, err := bridge.Tabs(ctx)
		return nil, browserTabsOutput{Tabs: tabs}, err
	}
}

type browserOpenInput struct {
	URL    string `json:"url" jsonschema:"absolute URL to open"`
	Active *bool  `json:"active,omitempty" jsonschema:"make the new tab active; defaults to true"`
}

func browserOpen(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, browserOpenInput) (*mcp.CallToolResult, browser.Tab, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browserOpenInput) (*mcp.CallToolResult, browser.Tab, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, browser.Tab{}, err
		}
		active := in.Active == nil || *in.Active
		result, err := bridge.Open(ctx, in.URL, active)
		return nil, result, err
	}
}

type browserTabInput struct {
	TabID int `json:"tab_id" jsonschema:"positive browser tab ID"`
}

type operationOutput struct {
	Success bool `json:"success"`
}

func browserClose(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, browserTabInput) (*mcp.CallToolResult, operationOutput, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browserTabInput) (*mcp.CallToolResult, operationOutput, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, operationOutput{}, err
		}
		err := bridge.CloseTab(ctx, in.TabID)
		return nil, operationOutput{Success: err == nil}, err
	}
}

type browserNavigateInput struct {
	TabID int    `json:"tab_id" jsonschema:"positive browser tab ID"`
	URL   string `json:"url" jsonschema:"absolute URL to navigate to"`
}

func browserNavigate(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, browserNavigateInput) (*mcp.CallToolResult, browser.Tab, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browserNavigateInput) (*mcp.CallToolResult, browser.Tab, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, browser.Tab{}, err
		}
		result, err := bridge.Navigate(ctx, in.TabID, in.URL)
		return nil, result, err
	}
}

type browserSnapshotInput struct {
	TabID       int `json:"tab_id" jsonschema:"positive browser tab ID"`
	MaxElements int `json:"max_elements,omitempty" jsonschema:"maximum interactive elements from 1 to 5000; defaults to 500"`
	MaxText     int `json:"max_text,omitempty" jsonschema:"maximum visible-text characters from 1 to 500000; defaults to 50000"`
}

func browserSnapshot(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, browserSnapshotInput) (*mcp.CallToolResult, browser.Snapshot, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browserSnapshotInput) (*mcp.CallToolResult, browser.Snapshot, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, browser.Snapshot{}, err
		}
		if in.MaxElements == 0 {
			in.MaxElements = 500
		}
		if in.MaxText == 0 {
			in.MaxText = 50000
		}
		if in.MaxElements < 1 || in.MaxElements > 5000 {
			return nil, browser.Snapshot{}, errors.New("max_elements must be between 1 and 5000")
		}
		if in.MaxText < 1 || in.MaxText > 500000 {
			return nil, browser.Snapshot{}, errors.New("max_text must be between 1 and 500000")
		}
		result, err := bridge.Snapshot(ctx, in.TabID, in.MaxElements, in.MaxText)
		return nil, result, err
	}
}

func browserScreenshot(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, browserTabInput) (*mcp.CallToolResult, browser.ScreenshotInfo, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browserTabInput) (*mcp.CallToolResult, browser.ScreenshotInfo, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, browser.ScreenshotInfo{}, err
		}
		data, info, err := bridge.Screenshot(ctx, in.TabID)
		if err != nil {
			return nil, browser.ScreenshotInfo{}, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: info.MIMEType}}}, info, nil
	}
}

func browserAction(bridge *browser.Bridge) func(context.Context, *mcp.CallToolRequest, browser.Action) (*mcp.CallToolResult, browser.ActionResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browser.Action) (*mcp.CallToolResult, browser.ActionResult, error) {
		if err := requireBridge(bridge); err != nil {
			return nil, browser.ActionResult{}, err
		}
		result, err := bridge.Act(ctx, in)
		return nil, result, err
	}
}

func computerScreenshot(controller computer.Controller) func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, computer.ScreenshotInfo, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, computer.ScreenshotInfo, error) {
		if controller == nil {
			return nil, computer.ScreenshotInfo{}, errors.New("computer control is unavailable on this platform")
		}
		data, info, err := controller.Screenshot(ctx)
		if err != nil {
			return nil, computer.ScreenshotInfo{}, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: info.MIMEType}}}, info, nil
	}
}

func computerAction(controller computer.Controller) func(context.Context, *mcp.CallToolRequest, computer.Action) (*mcp.CallToolResult, computer.ActionResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in computer.Action) (*mcp.CallToolResult, computer.ActionResult, error) {
		if controller == nil {
			return nil, computer.ActionResult{}, errors.New("computer control is unavailable on this platform")
		}
		result, err := controller.Act(ctx, in)
		return nil, result, err
	}
}
