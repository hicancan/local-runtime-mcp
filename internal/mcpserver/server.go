package mcpserver

import (
	"context"
	"fmt"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "2.0.0"

const instructions = "Local Runtime MCP operates on the machine where this server process runs. Filesystem, image, and process calls use configured roots and cannot escape them. Browser tools control the user's Chromium tabs through the authenticated bundled extension; computer tools operate the desktop through the configured OS backend. Prefer browser tools for web pages and computer tools for native UI. Read images and screenshots through their native image-content tools."

func New(runtime *core.Runtime, browserBridge *browser.Bridge, computerController computer.Controller) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "local-runtime-mcp", Version: Version},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{}},
	)

	mcp.AddTool(server, tool("filesystem_roots", "List filesystem roots", "List the configured filesystem roots available to all runtime tools.", true, false, true, false), filesystemRoots(runtime))
	mcp.AddTool(server, tool("filesystem_list", "List directory", "List files and directories below a path in a configured root without following symbolic links.", true, false, true, false), filesystemList(runtime))
	mcp.AddTool(server, tool("filesystem_stat", "Inspect path", "Inspect a file or directory. Regular files include MIME type and SHA-256 digest.", true, false, true, false), filesystemStat(runtime))
	mcp.AddTool(server, tool("filesystem_read_text", "Read text file", "Read bounded UTF-8 text and return the complete-file SHA-256 digest for concurrency checks.", true, false, true, false), filesystemReadText(runtime))
	mcp.AddTool(server, tool("filesystem_write_text", "Write text file", "Atomically create or replace a UTF-8 text file, optionally guarded by its previous SHA-256 digest.", false, true, true, false), filesystemWriteText(runtime))
	mcp.AddTool(server, tool("filesystem_edit_text", "Edit text file", "Atomically replace an exact UTF-8 text fragment, rejecting ambiguous matches by default.", false, true, true, false), filesystemEditText(runtime))
	mcp.AddTool(server, tool("filesystem_search_text", "Search text files", "Search bounded UTF-8 files below a path using literal text or a Go regular expression.", true, false, true, false), filesystemSearchText(runtime))
	mcp.AddTool(server, tool("image_read", "Read image", "Return PNG, JPEG, GIF, or WebP bytes as native MCP image content with image metadata.", true, false, true, false), imageRead(runtime))
	mcp.AddTool(server, tool("process_run", "Run process", "Run an installed program directly with an argument array, optional stdin, bounded output, and a root-relative directory.", false, true, false, true), processRun(runtime))
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

type rootsOutput struct {
	Roots []core.RootInfo `json:"roots"`
}

func filesystemRoots(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, rootsOutput, error) {
	return func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, rootsOutput, error) {
		return nil, rootsOutput{Roots: runtime.Roots()}, nil
	}
}

type listInput struct {
	Root          string `json:"root" jsonschema:"configured root name"`
	Path          string `json:"path,omitempty" jsonschema:"directory path relative to the root; defaults to the root"`
	MaxDepth      int    `json:"max_depth,omitempty" jsonschema:"maximum traversal depth from 1 to 64; defaults to 4"`
	IncludeHidden bool   `json:"include_hidden,omitempty" jsonschema:"include dotfiles and dot directories"`
}

type listOutput struct {
	Entries []core.FileEntry `json:"entries"`
}

func filesystemList(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, listInput) (*mcp.CallToolResult, listOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
		entries, err := runtime.List(in.Root, in.Path, in.MaxDepth, in.IncludeHidden)
		return nil, listOutput{Entries: entries}, err
	}
}

type pathInput struct {
	Root string `json:"root" jsonschema:"configured root name"`
	Path string `json:"path" jsonschema:"path relative to the root"`
}

func filesystemStat(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, pathInput) (*mcp.CallToolResult, core.FileInfo, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, core.FileInfo, error) {
		result, err := runtime.Stat(in.Root, in.Path)
		return nil, result, err
	}
}

type readTextInput struct {
	Root     string `json:"root" jsonschema:"configured root name"`
	Path     string `json:"path" jsonschema:"text file path relative to the root"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes returned from 1 to 8388608; defaults to 1048576"`
}

func filesystemReadText(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, readTextInput) (*mcp.CallToolResult, core.TextReadResult, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in readTextInput) (*mcp.CallToolResult, core.TextReadResult, error) {
		result, err := runtime.ReadText(in.Root, in.Path, in.MaxBytes)
		return nil, result, err
	}
}

type writeTextInput struct {
	Root           string `json:"root" jsonschema:"configured root name"`
	Path           string `json:"path" jsonschema:"text file path relative to the root"`
	Content        string `json:"content" jsonschema:"complete UTF-8 file content"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"optional SHA-256 digest that the existing file must match"`
	CreateOnly     bool   `json:"create_only,omitempty" jsonschema:"fail if the target already exists"`
}

func filesystemWriteText(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, writeTextInput) (*mcp.CallToolResult, core.TextWriteResult, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in writeTextInput) (*mcp.CallToolResult, core.TextWriteResult, error) {
		result, err := runtime.WriteText(in.Root, in.Path, in.Content, in.ExpectedSHA256, in.CreateOnly)
		return nil, result, err
	}
}

type editTextInput struct {
	Root           string `json:"root" jsonschema:"configured root name"`
	Path           string `json:"path" jsonschema:"text file path relative to the root"`
	OldText        string `json:"old_text" jsonschema:"exact existing UTF-8 text to replace"`
	NewText        string `json:"new_text" jsonschema:"replacement UTF-8 text"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"optional SHA-256 digest that the file must match"`
	ReplaceAll     bool   `json:"replace_all,omitempty" jsonschema:"replace every match instead of requiring exactly one"`
}

func filesystemEditText(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, editTextInput) (*mcp.CallToolResult, core.TextEditResult, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in editTextInput) (*mcp.CallToolResult, core.TextEditResult, error) {
		result, err := runtime.EditText(in.Root, in.Path, in.OldText, in.NewText, in.ExpectedSHA256, in.ReplaceAll)
		return nil, result, err
	}
}

type searchTextInput struct {
	Root          string `json:"root" jsonschema:"configured root name"`
	Path          string `json:"path,omitempty" jsonschema:"file or directory path relative to the root; defaults to the root"`
	Query         string `json:"query" jsonschema:"literal text or Go regular expression to find"`
	Regex         bool   `json:"regex,omitempty" jsonschema:"interpret query as a Go regular expression"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" jsonschema:"perform a case-sensitive search"`
	IncludeHidden bool   `json:"include_hidden,omitempty" jsonschema:"include dotfiles and dot directories"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema:"maximum matches from 1 to 10000; defaults to 200"`
}

type searchTextOutput struct {
	Matches []core.SearchMatch `json:"matches"`
}

func filesystemSearchText(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, searchTextInput) (*mcp.CallToolResult, searchTextOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in searchTextInput) (*mcp.CallToolResult, searchTextOutput, error) {
		matches, err := runtime.SearchText(in.Root, in.Path, in.Query, in.Regex, in.CaseSensitive, in.IncludeHidden, in.MaxResults)
		return nil, searchTextOutput{Matches: matches}, err
	}
}

type imageReadInput struct {
	Root     string `json:"root" jsonschema:"configured root name"`
	Path     string `json:"path" jsonschema:"image path relative to the root"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"maximum complete image size from 1 to 67108864; defaults to 10485760"`
}

func imageRead(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, imageReadInput) (*mcp.CallToolResult, core.ImageReadResult, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in imageReadInput) (*mcp.CallToolResult, core.ImageReadResult, error) {
		data, metadata, err := runtime.ReadImage(in.Root, in.Path, in.MaxBytes)
		if err != nil {
			return nil, core.ImageReadResult{}, err
		}
		result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: metadata.MIMEType}}}
		return result, metadata, nil
	}
}

type processRunInput struct {
	Root           string            `json:"root" jsonschema:"configured root name"`
	Program        string            `json:"program" jsonschema:"installed program name or executable path; no shell parsing is performed"`
	Args           []string          `json:"args,omitempty" jsonschema:"program argument array"`
	Directory      string            `json:"directory,omitempty" jsonschema:"working directory relative to the root; defaults to the root"`
	Environment    map[string]string `json:"environment,omitempty" jsonschema:"environment variables added or overridden for the child process"`
	Stdin          string            `json:"stdin,omitempty" jsonschema:"text sent to standard input"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"timeout from 1 to 86400 seconds; defaults to 300"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"separate stdout and stderr limit from 1 to 16777216 bytes; defaults to 1048576"`
}

func processRun(runtime *core.Runtime) func(context.Context, *mcp.CallToolRequest, processRunInput) (*mcp.CallToolResult, core.ProcessResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in processRunInput) (*mcp.CallToolResult, core.ProcessResult, error) {
		result, err := runtime.RunProcess(ctx, in.Root, core.ProcessOptions{
			Program: in.Program, Args: in.Args, Directory: in.Directory, Environment: in.Environment,
			Stdin: in.Stdin, TimeoutSeconds: in.TimeoutSeconds, MaxOutputBytes: in.MaxOutputBytes,
		})
		if err != nil {
			return nil, result, fmt.Errorf("run process: %w", err)
		}
		return nil, result, nil
	}
}
