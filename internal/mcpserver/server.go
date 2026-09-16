package mcpserver

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	runtimeimage "github.com/hicancan/local-runtime-mcp/internal/image"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "6.0.1"

const instructions = "Local Runtime MCP exposes the machine where lrmcp is running. Filesystem and image tools accept direct absolute paths or paths relative to the server process. Process tools execute installed programs directly without shell parsing and return sessions for longer programs. Browser tools use the bundled Chromium extension over an authenticated loopback bridge. Computer tools operate the current interactive desktop and target open windows. Prefer browser tools for web pages, computer tools for native UI, and native image-content tools for images and screenshots. Source code is available under AGPL-3.0-only at https://github.com/hicancan/local-runtime-mcp."

func New(ctx context.Context, browserBridge *browser.Bridge, computerController computer.Controller) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "local-runtime-mcp", Version: Version},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{}},
	)

	mcp.AddTool(server, tool("filesystem_list", "List directory", "List a bounded number of files and directories below a direct machine path without following symbolic links.", true, false, true, false), filesystemList)
	mcp.AddTool(server, tool("filesystem_stat", "Inspect path", "Inspect a direct path without following symbolic links; optionally calculate a regular file's SHA-256.", true, false, true, false), filesystemStat)
	mcp.AddTool(server, tool("filesystem_read_text", "Read text file", "Read bounded complete UTF-8 lines from a direct machine path, starting at any one-based line.", true, false, true, false), filesystemReadText)
	mcp.AddTool(server, tool("filesystem_write_text", "Write text file", "Atomically create or replace a complete UTF-8 text file, optionally requiring an expected SHA-256.", false, true, true, false), filesystemWriteText)
	mcp.AddTool(server, tool("filesystem_patch_text", "Patch text file", "Apply uniquely anchored, non-overlapping UTF-8 hunks against one expected file version and commit them atomically.", false, true, true, false), filesystemPatchText)
	mcp.AddTool(server, tool("filesystem_search_text", "Search text files", "Search bounded UTF-8 files below a direct path using literal text or a Go regular expression.", true, false, true, false), filesystemSearchText)
	mcp.AddTool(server, tool("image_read", "Read image", "Return a direct PNG, JPEG, GIF, or WebP path as native MCP image content with metadata.", true, false, true, false), imageRead)

	processManager := runtimeprocess.NewManager(ctx)
	processRunTool := inputTool[processRunInput](tool("process_run", "Run process", "Start a program directly in pipe or interactive PTY mode. Completed programs return their result; longer programs return a session ID.", false, true, false, true), func(schema *jsonschema.Schema) {
		schema.Properties["io_mode"].Enum = enum("pipe", "pty")
	})
	mcp.AddTool(server, processRunTool, processRun(processManager))
	mcp.AddTool(server, tool("process_continue", "Continue process", "Read incremental output, write or close pipe stdin, resize a PTY, wait for, or terminate a process session.", false, true, false, true), processContinue(processManager))

	registerBrowserTools(server, browserBridge)
	registerComputerTools(server, computerController)
	return server
}

func tool(name, title, description string, readOnly, destructive, idempotent, openWorld bool) *mcp.Tool {
	return &mcp.Tool{
		Name: name, Title: title, Description: description,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: readOnly, DestructiveHint: pointer(destructive),
			IdempotentHint: idempotent, OpenWorldHint: pointer(openWorld),
		},
	}
}

func pointer(value bool) *bool { return &value }

func inputTool[Input any](value *mcp.Tool, configure func(*jsonschema.Schema)) *mcp.Tool {
	schema, err := jsonschema.For[Input](nil)
	if err != nil {
		panic(fmt.Sprintf("create input schema for %s: %v", value.Name, err))
	}
	configure(schema)
	value.InputSchema = schema
	return value
}

func enum(values ...string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

type emptyInput struct{}

type listInput struct {
	Path          string `json:"path" jsonschema:"absolute directory path or path relative to the server process"`
	MaxDepth      int    `json:"max_depth,omitempty" jsonschema:"maximum traversal depth from 1 to 64; defaults to 1"`
	IncludeHidden bool   `json:"include_hidden,omitempty" jsonschema:"include dotfiles and dot directories"`
	MaxEntries    int    `json:"max_entries,omitempty" jsonschema:"maximum returned entries from 1 to 100000; defaults to 1000"`
}

func filesystemList(_ context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, filesystem.ListResult, error) {
	result, err := filesystem.List(in.Path, in.MaxDepth, in.IncludeHidden, in.MaxEntries)
	return nil, result, err
}

type pathInput struct {
	Path       string `json:"path" jsonschema:"absolute path or path relative to the server process"`
	WithSHA256 bool   `json:"with_sha256,omitempty" jsonschema:"calculate SHA-256 for a regular file by reading it completely"`
}

func filesystemStat(_ context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, filesystem.FileInfo, error) {
	result, err := filesystem.Stat(in.Path, in.WithSHA256)
	return nil, result, err
}

type readTextInput struct {
	Path       string `json:"path" jsonschema:"absolute text-file path or path relative to the server process"`
	StartLine  int    `json:"start_line,omitempty" jsonschema:"one-based first line; defaults to 1"`
	LineCount  int    `json:"line_count,omitempty" jsonschema:"maximum complete lines returned; zero means until max_bytes or EOF"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes returned from 1 to 8388608; defaults to 1048576"`
	WithSHA256 bool   `json:"with_sha256,omitempty" jsonschema:"calculate SHA-256 by reading the entire file"`
}

func filesystemReadText(_ context.Context, _ *mcp.CallToolRequest, in readTextInput) (*mcp.CallToolResult, filesystem.TextReadResult, error) {
	result, err := filesystem.ReadText(in.Path, filesystem.ReadTextOptions{StartLine: in.StartLine, LineCount: in.LineCount, MaxBytes: in.MaxBytes, WithSHA256: in.WithSHA256})
	return nil, result, err
}

type writeTextInput struct {
	Path           string `json:"path" jsonschema:"absolute text-file path or path relative to the server process"`
	Content        string `json:"content" jsonschema:"complete UTF-8 file content"`
	CreateOnly     bool   `json:"create_only,omitempty" jsonschema:"fail if the target already exists"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"replace only when the existing file has this SHA-256; mutually exclusive with create_only"`
	CreateParents  bool   `json:"create_parents,omitempty" jsonschema:"create missing parent directories; defaults to false"`
}

func filesystemWriteText(_ context.Context, _ *mcp.CallToolRequest, in writeTextInput) (*mcp.CallToolResult, filesystem.TextWriteResult, error) {
	if in.CreateOnly && in.ExpectedSHA256 != "" {
		return nil, filesystem.TextWriteResult{}, fmt.Errorf("create_only and expected_sha256 are mutually exclusive")
	}
	result, err := filesystem.WriteText(in.Path, in.Content, filesystem.WriteTextOptions{CreateOnly: in.CreateOnly, ExpectedSHA256: in.ExpectedSHA256, CreateParents: in.CreateParents})
	return nil, result, err
}

type patchTextInput struct {
	Path           string                     `json:"path" jsonschema:"absolute text-file path or path relative to the server process"`
	Hunks          []filesystem.TextPatchHunk `json:"hunks" jsonschema:"exact contextual hunks, all located against the same original file"`
	ExpectedSHA256 string                     `json:"expected_sha256" jsonschema:"required SHA-256 of the original file"`
}

func filesystemPatchText(_ context.Context, _ *mcp.CallToolRequest, in patchTextInput) (*mcp.CallToolResult, filesystem.TextPatchResult, error) {
	result, err := filesystem.PatchText(in.Path, filesystem.PatchTextOptions{Hunks: in.Hunks, ExpectedSHA256: in.ExpectedSHA256})
	return nil, result, err
}

type searchTextInput struct {
	Path          string `json:"path" jsonschema:"absolute file/directory path or path relative to the server process"`
	Query         string `json:"query" jsonschema:"literal text or Go regular expression to find"`
	Regex         bool   `json:"regex,omitempty" jsonschema:"interpret query as a Go regular expression"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" jsonschema:"perform a case-sensitive search"`
	IncludeHidden bool   `json:"include_hidden,omitempty" jsonschema:"include dotfiles and dot directories"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema:"maximum matches from 1 to 10000; defaults to 200"`
}

func filesystemSearchText(_ context.Context, _ *mcp.CallToolRequest, in searchTextInput) (*mcp.CallToolResult, filesystem.SearchResult, error) {
	result, err := filesystem.SearchText(in.Path, in.Query, in.Regex, in.CaseSensitive, in.IncludeHidden, in.MaxResults)
	return nil, result, err
}

type imageReadInput struct {
	Path       string `json:"path" jsonschema:"absolute image path or path relative to the server process"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"maximum complete image size from 1 to 67108864; defaults to 10485760"`
	MaxPixels  int    `json:"max_pixels,omitempty" jsonschema:"maximum width times height from 1 to 250000000; defaults to 40000000"`
	CropX      int    `json:"crop_x,omitempty" jsonschema:"non-negative source X coordinate; requires crop_width and crop_height"`
	CropY      int    `json:"crop_y,omitempty" jsonschema:"non-negative source Y coordinate; requires crop_width and crop_height"`
	CropWidth  int    `json:"crop_width,omitempty" jsonschema:"positive source crop width; requires crop_height"`
	CropHeight int    `json:"crop_height,omitempty" jsonschema:"positive source crop height; requires crop_width"`
	MaxWidth   int    `json:"max_width,omitempty" jsonschema:"maximum projected width from 1 to 32768; aspect ratio is preserved"`
	MaxHeight  int    `json:"max_height,omitempty" jsonschema:"maximum projected height from 1 to 32768; aspect ratio is preserved"`
}

func imageRead(_ context.Context, _ *mcp.CallToolRequest, in imageReadInput) (*mcp.CallToolResult, runtimeimage.ReadResult, error) {
	data, metadata, err := runtimeimage.Read(in.Path, runtimeimage.ReadOptions{MaxBytes: in.MaxBytes, MaxPixels: in.MaxPixels, CropX: in.CropX, CropY: in.CropY, CropWidth: in.CropWidth, CropHeight: in.CropHeight, MaxWidth: in.MaxWidth, MaxHeight: in.MaxHeight})
	if err != nil {
		return nil, runtimeimage.ReadResult{}, err
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: metadata.MIMEType}}}
	return result, metadata, nil
}

type processRunInput struct {
	Program        string            `json:"program" jsonschema:"installed program name or executable path; no shell parsing is performed"`
	Args           []string          `json:"args,omitempty" jsonschema:"program argument array"`
	Directory      string            `json:"directory,omitempty" jsonschema:"absolute working directory or path relative to the server process; defaults to the server working directory"`
	Environment    map[string]string `json:"environment,omitempty" jsonschema:"environment variables added or overridden for the child process"`
	Stdin          string            `json:"stdin,omitempty" jsonschema:"initial text sent to standard input"`
	KeepStdinOpen  bool              `json:"keep_stdin_open,omitempty" jsonschema:"keep stdin open so process_continue can write more input"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"lifetime timeout from 1 to 86400 seconds; defaults to 300"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"separate retained stdout and stderr limit from 1 to 16777216 bytes; defaults to 1048576"`
	YieldTimeMS    int               `json:"yield_time_ms,omitempty" jsonschema:"initial wait from 1 to 60000 milliseconds; defaults to 10000"`
	IOMode         string            `json:"io_mode,omitempty" jsonschema:"pipe for separate stdout and stderr, or pty for one interactive terminal stream; defaults to pipe"`
	Columns        int               `json:"columns,omitempty" jsonschema:"PTY width from 1 to 1000; defaults to 80"`
	Rows           int               `json:"rows,omitempty" jsonschema:"PTY height from 1 to 1000; defaults to 25"`
}

func processRun(manager *runtimeprocess.Manager) func(context.Context, *mcp.CallToolRequest, processRunInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in processRunInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
		result, err := manager.Run(ctx, runtimeprocess.Options{
			Program: in.Program, Args: in.Args, Directory: in.Directory, Environment: in.Environment,
			Stdin: in.Stdin, KeepStdinOpen: in.KeepStdinOpen, TimeoutSeconds: in.TimeoutSeconds,
			MaxOutputBytes: in.MaxOutputBytes, YieldTimeMS: in.YieldTimeMS,
			IOMode: in.IOMode, Columns: in.Columns, Rows: in.Rows,
		})
		if err != nil {
			return nil, result, fmt.Errorf("run process: %w", err)
		}
		return nil, result, nil
	}
}

type processContinueInput struct {
	SessionID   string `json:"session_id" jsonschema:"process session ID returned by process_run"`
	Stdin       string `json:"stdin,omitempty" jsonschema:"additional text written to the open process stdin"`
	CloseStdin  bool   `json:"close_stdin,omitempty" jsonschema:"close the process stdin after writing"`
	Terminate   bool   `json:"terminate,omitempty" jsonschema:"terminate the complete process tree"`
	YieldTimeMS int    `json:"yield_time_ms,omitempty" jsonschema:"wait for output or exit from 1 to 60000 milliseconds; defaults to 1000"`
	Columns     int    `json:"columns,omitempty" jsonschema:"new PTY width from 1 to 1000; requires rows"`
	Rows        int    `json:"rows,omitempty" jsonschema:"new PTY height from 1 to 1000; requires columns"`
}

func processContinue(manager *runtimeprocess.Manager) func(context.Context, *mcp.CallToolRequest, processContinueInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in processContinueInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
		result, err := manager.Continue(ctx, runtimeprocess.ContinueOptions{
			SessionID: in.SessionID, Stdin: in.Stdin, CloseStdin: in.CloseStdin, Terminate: in.Terminate, YieldTimeMS: in.YieldTimeMS, Columns: in.Columns, Rows: in.Rows,
		})
		if err != nil {
			return nil, result, fmt.Errorf("continue process: %w", err)
		}
		return nil, result, nil
	}
}
