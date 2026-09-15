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

const Version = "5.0.0"

const instructions = "Local Runtime MCP exposes the machine where lrmcp is running. Filesystem and image tools accept direct absolute paths or paths relative to the server process. Process tools execute installed programs directly without shell parsing and return sessions for longer programs. Browser tools use the bundled Chromium extension over an authenticated loopback bridge. Computer tools operate the current interactive desktop and target open windows. Prefer browser tools for web pages, computer tools for native UI, and native image-content tools for images and screenshots."

func New(ctx context.Context, browserBridge *browser.Bridge, computerController computer.Controller) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "local-runtime-mcp", Version: Version},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{}},
	)

	mcp.AddTool(server, tool("filesystem_list", "List directory", "List a bounded number of files and directories below a direct machine path without following symbolic links.", true, false, true, false), filesystemList)
	mcp.AddTool(server, tool("filesystem_stat", "Inspect path", "Inspect a direct path without following symbolic links; optionally calculate a regular file's SHA-256.", true, false, true, false), filesystemStat)
	mcp.AddTool(server, tool("filesystem_read_text", "Read text file", "Read bounded complete UTF-8 lines from a direct machine path, starting at any one-based line.", true, false, true, false), filesystemReadText)
	mcp.AddTool(server, tool("filesystem_write_text", "Write text file", "Atomically create or replace a complete UTF-8 text file, optionally requiring an expected SHA-256.", false, true, true, false), filesystemWriteText)
	mcp.AddTool(server, tool("filesystem_edit_text", "Edit text file", "Validate ordered exact UTF-8 replacements in memory and commit all of them together.", false, true, true, false), filesystemEditText)
	mcp.AddTool(server, tool("filesystem_search_text", "Search text files", "Search bounded UTF-8 files below a direct path using literal text or a Go regular expression.", true, false, true, false), filesystemSearchText)
	mcp.AddTool(server, tool("image_read", "Read image", "Return a direct PNG, JPEG, GIF, or WebP path as native MCP image content with metadata.", true, false, true, false), imageRead)

	processManager := runtimeprocess.NewManager(ctx)
	mcp.AddTool(server, tool("process_run", "Run process", "Start an installed program directly with an argument array. Completed programs return their result; longer programs return a session ID for process_continue.", false, true, false, true), processRun(processManager))
	mcp.AddTool(server, tool("process_continue", "Continue process", "Read incremental output, write or close stdin, wait for, or terminate a process session returned by process_run.", false, true, false, true), processContinue(processManager))

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

type editTextInput struct {
	Path           string                `json:"path" jsonschema:"absolute text-file path or path relative to the server process"`
	Edits          []filesystem.TextEdit `json:"edits" jsonschema:"ordered exact replacements validated in memory and committed together"`
	ExpectedSHA256 string                `json:"expected_sha256,omitempty" jsonschema:"edit only when the existing file has this SHA-256"`
}

func filesystemEditText(_ context.Context, _ *mcp.CallToolRequest, in editTextInput) (*mcp.CallToolResult, filesystem.TextEditResult, error) {
	result, err := filesystem.EditText(in.Path, filesystem.EditTextOptions{Edits: in.Edits, ExpectedSHA256: in.ExpectedSHA256})
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
	Path      string `json:"path" jsonschema:"absolute image path or path relative to the server process"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"maximum complete image size from 1 to 67108864; defaults to 10485760"`
	MaxPixels int    `json:"max_pixels,omitempty" jsonschema:"maximum width times height from 1 to 250000000; defaults to 40000000"`
}

func imageRead(_ context.Context, _ *mcp.CallToolRequest, in imageReadInput) (*mcp.CallToolResult, runtimeimage.ReadResult, error) {
	data, metadata, err := runtimeimage.Read(in.Path, in.MaxBytes, in.MaxPixels)
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
}

func processRun(manager *runtimeprocess.Manager) func(context.Context, *mcp.CallToolRequest, processRunInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in processRunInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
		result, err := manager.Run(ctx, runtimeprocess.Options{
			Program: in.Program, Args: in.Args, Directory: in.Directory, Environment: in.Environment,
			Stdin: in.Stdin, KeepStdinOpen: in.KeepStdinOpen, TimeoutSeconds: in.TimeoutSeconds,
			MaxOutputBytes: in.MaxOutputBytes, YieldTimeMS: in.YieldTimeMS,
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
}

func processContinue(manager *runtimeprocess.Manager) func(context.Context, *mcp.CallToolRequest, processContinueInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in processContinueInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
		result, err := manager.Continue(ctx, runtimeprocess.ContinueOptions{
			SessionID: in.SessionID, Stdin: in.Stdin, CloseStdin: in.CloseStdin, Terminate: in.Terminate, YieldTimeMS: in.YieldTimeMS,
		})
		if err != nil {
			return nil, result, fmt.Errorf("continue process: %w", err)
		}
		return nil, result, nil
	}
}
