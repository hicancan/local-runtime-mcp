package mcpserver

import (
	"context"
	"fmt"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	runtimeimage "github.com/hicancan/local-runtime-mcp/internal/image"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "3.0.0"

const instructions = "Local Runtime MCP exposes the machine where lrmcp is running. Filesystem and image tools accept direct absolute paths or paths relative to the server process. Process tools execute installed programs directly without shell parsing. Browser tools use the bundled Chromium extension over an authenticated loopback bridge. Computer tools operate the current interactive desktop. Prefer browser tools for web pages, computer tools for native UI, and native image-content tools for images and screenshots."

func New(browserBridge *browser.Bridge, computerController computer.Controller) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "local-runtime-mcp", Version: Version},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{}},
	)

	mcp.AddTool(server, tool("filesystem_list", "List directory", "List files and directories below a direct machine path without following symbolic links.", true, false, true, false), filesystemList)
	mcp.AddTool(server, tool("filesystem_stat", "Inspect path", "Inspect a direct file or directory path and report file MIME type when applicable.", true, false, true, false), filesystemStat)
	mcp.AddTool(server, tool("filesystem_read_text", "Read text file", "Read bounded UTF-8 text from a direct machine path.", true, false, true, false), filesystemReadText)
	mcp.AddTool(server, tool("filesystem_write_text", "Write text file", "Atomically create or replace a UTF-8 text file at a direct machine path.", false, true, true, false), filesystemWriteText)
	mcp.AddTool(server, tool("filesystem_edit_text", "Edit text file", "Atomically replace an exact UTF-8 text fragment, rejecting ambiguous matches by default.", false, true, true, false), filesystemEditText)
	mcp.AddTool(server, tool("filesystem_search_text", "Search text files", "Search bounded UTF-8 files below a direct path using literal text or a Go regular expression.", true, false, true, false), filesystemSearchText)
	mcp.AddTool(server, tool("image_read", "Read image", "Return a direct PNG, JPEG, GIF, or WebP path as native MCP image content with metadata.", true, false, true, false), imageRead)
	mcp.AddTool(server, tool("process_run", "Run process", "Run an installed program directly with an argument array, optional stdin, working directory, bounded output, and timeout.", false, true, false, true), processRun)
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

type emptyInput struct{}

type listInput struct {
	Path          string `json:"path" jsonschema:"absolute directory path or path relative to the server process"`
	MaxDepth      int    `json:"max_depth,omitempty" jsonschema:"maximum traversal depth from 1 to 64; defaults to 4"`
	IncludeHidden bool   `json:"include_hidden,omitempty" jsonschema:"include dotfiles and dot directories"`
}

type listOutput struct {
	Entries []filesystem.FileEntry `json:"entries"`
}

func filesystemList(_ context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
	entries, err := filesystem.List(in.Path, in.MaxDepth, in.IncludeHidden)
	return nil, listOutput{Entries: entries}, err
}

type pathInput struct {
	Path string `json:"path" jsonschema:"absolute path or path relative to the server process"`
}

func filesystemStat(_ context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, filesystem.FileInfo, error) {
	result, err := filesystem.Stat(in.Path)
	return nil, result, err
}

type readTextInput struct {
	Path     string `json:"path" jsonschema:"absolute text-file path or path relative to the server process"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes returned from 1 to 8388608; defaults to 1048576"`
}

func filesystemReadText(_ context.Context, _ *mcp.CallToolRequest, in readTextInput) (*mcp.CallToolResult, filesystem.TextReadResult, error) {
	result, err := filesystem.ReadText(in.Path, in.MaxBytes)
	return nil, result, err
}

type writeTextInput struct {
	Path       string `json:"path" jsonschema:"absolute text-file path or path relative to the server process"`
	Content    string `json:"content" jsonschema:"complete UTF-8 file content"`
	CreateOnly bool   `json:"create_only,omitempty" jsonschema:"fail if the target already exists"`
}

func filesystemWriteText(_ context.Context, _ *mcp.CallToolRequest, in writeTextInput) (*mcp.CallToolResult, filesystem.TextWriteResult, error) {
	result, err := filesystem.WriteText(in.Path, in.Content, in.CreateOnly)
	return nil, result, err
}

type editTextInput struct {
	Path       string `json:"path" jsonschema:"absolute text-file path or path relative to the server process"`
	OldText    string `json:"old_text" jsonschema:"exact existing UTF-8 text to replace"`
	NewText    string `json:"new_text" jsonschema:"replacement UTF-8 text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every match instead of requiring exactly one"`
}

func filesystemEditText(_ context.Context, _ *mcp.CallToolRequest, in editTextInput) (*mcp.CallToolResult, filesystem.TextEditResult, error) {
	result, err := filesystem.EditText(in.Path, in.OldText, in.NewText, in.ReplaceAll)
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

type searchTextOutput struct {
	Matches []filesystem.SearchMatch `json:"matches"`
}

func filesystemSearchText(_ context.Context, _ *mcp.CallToolRequest, in searchTextInput) (*mcp.CallToolResult, searchTextOutput, error) {
	matches, err := filesystem.SearchText(in.Path, in.Query, in.Regex, in.CaseSensitive, in.IncludeHidden, in.MaxResults)
	return nil, searchTextOutput{Matches: matches}, err
}

type imageReadInput struct {
	Path     string `json:"path" jsonschema:"absolute image path or path relative to the server process"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"maximum complete image size from 1 to 67108864; defaults to 10485760"`
}

func imageRead(_ context.Context, _ *mcp.CallToolRequest, in imageReadInput) (*mcp.CallToolResult, runtimeimage.ReadResult, error) {
	data, metadata, err := runtimeimage.Read(in.Path, in.MaxBytes)
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
	Stdin          string            `json:"stdin,omitempty" jsonschema:"text sent to standard input"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"timeout from 1 to 86400 seconds; defaults to 300"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"separate stdout and stderr limit from 1 to 16777216 bytes; defaults to 1048576"`
}

func processRun(ctx context.Context, _ *mcp.CallToolRequest, in processRunInput) (*mcp.CallToolResult, runtimeprocess.Result, error) {
	result, err := runtimeprocess.Run(ctx, runtimeprocess.Options{
		Program: in.Program, Args: in.Args, Directory: in.Directory, Environment: in.Environment,
		Stdin: in.Stdin, TimeoutSeconds: in.TimeoutSeconds, MaxOutputBytes: in.MaxOutputBytes,
	})
	if err != nil {
		return nil, result, fmt.Errorf("run process: %w", err)
	}
	return nil, result, nil
}
