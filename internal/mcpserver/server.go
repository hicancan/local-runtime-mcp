package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hicancan/workspace-mcp/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "1.0.0"

// New creates a Workspace MCP server with the complete tool set.
func New(manager *workspace.Manager) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: Version}, nil)

	mcp.AddTool(server, tool("workspace_list", "List workspaces", "List available workspaces and their detected capabilities.", true, false, true, false), workspaceList(manager))
	mcp.AddTool(server, tool("workspace_info", "Inspect workspace", "Describe one workspace and its detected capabilities.", true, false, true, false), workspaceInfo(manager))
	mcp.AddTool(server, tool("file_tree", "List files", "List files and directories inside a workspace.", true, false, true, false), fileTree(manager))
	mcp.AddTool(server, tool("file_info", "Inspect file", "Inspect a file or directory, including its MIME type and SHA-256 digest.", true, false, true, false), fileInfo(manager))
	mcp.AddTool(server, tool("file_read", "Read text file", "Read a UTF-8 text file from a workspace with its SHA-256 digest.", true, false, true, false), fileRead(manager))
	mcp.AddTool(server, tool("file_write", "Write text file", "Create or fully replace a UTF-8 text file in a workspace.", false, true, true, false), fileWrite(manager))
	mcp.AddTool(server, tool("image_read", "Read image", "Return a PNG, JPEG, GIF, or WebP file as MCP image content so the model can inspect it.", true, false, true, false), imageRead(manager))
	mcp.AddTool(server, tool("file_search", "Search files", "Search text files throughout a workspace using text or a Go regular expression.", true, false, true, false), fileSearch(manager))
	mcp.AddTool(server, tool("git_status", "Get Git status", "Return machine-readable Git branch and working-tree status for a workspace.", true, false, true, false), gitStatus(manager))
	mcp.AddTool(server, tool("git_diff", "Get Git diff", "Return the working-tree or staged Git diff for a workspace.", true, false, true, false), gitDiff(manager))
	mcp.AddTool(server, tool("git_pull", "Pull Git changes", "Pull changes into the workspace using the installed Git CLI.", false, true, false, false), gitPull(manager))
	mcp.AddTool(server, tool("git_commit", "Commit Git changes", "Optionally stage all changes and create a Git commit in the workspace.", false, false, false, false), gitCommit(manager))
	mcp.AddTool(server, tool("git_push", "Push Git changes", "Push workspace commits using the installed Git CLI and configured credentials.", false, true, false, false), gitPush(manager))
	mcp.AddTool(server, tool("exec", "Run command", "Run a local CLI program with arguments and the workspace root as its working directory.", false, true, false, true), runExec(manager))

	return server
}

func tool(name, title, description string, readOnly, destructive, idempotent, openWorld bool) *mcp.Tool {
	return &mcp.Tool{Name: name, Title: title, Description: description, Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: readOnly, DestructiveHint: boolPointer(destructive),
		IdempotentHint: idempotent, OpenWorldHint: boolPointer(openWorld),
	}}
}

func boolPointer(value bool) *bool {
	return &value
}

type EmptyInput struct{}

type WorkspaceListOutput struct {
	Workspaces []workspace.Info `json:"workspaces"`
}

func workspaceList(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, EmptyInput) (*mcp.CallToolResult, WorkspaceListOutput, error) {
	return func(context.Context, *mcp.CallToolRequest, EmptyInput) (*mcp.CallToolResult, WorkspaceListOutput, error) {
		return nil, WorkspaceListOutput{Workspaces: manager.List()}, nil
	}
}

type WorkspaceInput struct {
	Workspace string `json:"workspace" jsonschema:"workspace name"`
}

type WorkspaceInfoOutput struct {
	Workspace workspace.Info `json:"workspace"`
}

func workspaceInfo(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, WorkspaceInput) (*mcp.CallToolResult, WorkspaceInfoOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in WorkspaceInput) (*mcp.CallToolResult, WorkspaceInfoOutput, error) {
		if _, err := manager.Root(in.Workspace); err != nil {
			return nil, WorkspaceInfoOutput{}, err
		}
		return nil, WorkspaceInfoOutput{Workspace: manager.Info(in.Workspace)}, nil
	}
}

type FileTreeInput struct {
	Workspace     string `json:"workspace" jsonschema:"configured workspace name"`
	Path          string `json:"path,omitempty" jsonschema:"directory path relative to the workspace; defaults to the root"`
	MaxDepth      int    `json:"max_depth,omitempty" jsonschema:"maximum directory depth; defaults to 4"`
	IncludeHidden bool   `json:"include_hidden,omitempty" jsonschema:"include dotfiles and dot directories"`
}

type FileTreeOutput struct {
	Entries []workspace.TreeEntry `json:"entries"`
}

func fileTree(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, FileTreeInput) (*mcp.CallToolResult, FileTreeOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in FileTreeInput) (*mcp.CallToolResult, FileTreeOutput, error) {
		entries, err := manager.Tree(in.Workspace, in.Path, in.MaxDepth, in.IncludeHidden)
		return nil, FileTreeOutput{Entries: entries}, err
	}
}

type FileReadInput struct {
	Workspace string `json:"workspace" jsonschema:"configured workspace name"`
	Path      string `json:"path" jsonschema:"file path relative to the workspace"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes returned; defaults to 1048576"`
}

type FilePathInput struct {
	Workspace string `json:"workspace" jsonschema:"configured workspace name"`
	Path      string `json:"path" jsonschema:"file or directory path relative to the workspace"`
}

func fileInfo(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, FilePathInput) (*mcp.CallToolResult, workspace.FileInfo, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in FilePathInput) (*mcp.CallToolResult, workspace.FileInfo, error) {
		out, err := manager.Inspect(in.Workspace, in.Path)
		return nil, out, err
	}
}

func fileRead(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, FileReadInput) (*mcp.CallToolResult, workspace.ReadResult, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in FileReadInput) (*mcp.CallToolResult, workspace.ReadResult, error) {
		out, err := manager.Read(in.Workspace, in.Path, in.MaxBytes)
		return nil, out, err
	}
}

type FileWriteInput struct {
	Workspace string `json:"workspace" jsonschema:"configured workspace name"`
	Path      string `json:"path" jsonschema:"file path relative to the workspace"`
	Content   string `json:"content" jsonschema:"complete UTF-8 file content"`
}

func fileWrite(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, FileWriteInput) (*mcp.CallToolResult, workspace.WriteResult, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in FileWriteInput) (*mcp.CallToolResult, workspace.WriteResult, error) {
		if strings.TrimSpace(in.Path) == "" {
			return nil, workspace.WriteResult{}, errors.New("path cannot be empty")
		}
		out, err := manager.Write(in.Workspace, in.Path, in.Content)
		return nil, out, err
	}
}

type ImageReadInput struct {
	Workspace string `json:"workspace" jsonschema:"configured workspace name"`
	Path      string `json:"path" jsonschema:"image file path relative to the workspace"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"maximum complete image size returned; defaults to 10485760"`
}

func imageRead(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, ImageReadInput) (*mcp.CallToolResult, workspace.ImageReadResult, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in ImageReadInput) (*mcp.CallToolResult, workspace.ImageReadResult, error) {
		data, out, err := manager.ReadImage(in.Workspace, in.Path, in.MaxBytes)
		if err != nil {
			return nil, workspace.ImageReadResult{}, err
		}
		result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: out.MIMEType}}}
		return result, out, nil
	}
}

type FileSearchInput struct {
	Workspace     string `json:"workspace" jsonschema:"configured workspace name"`
	Query         string `json:"query" jsonschema:"text or Go regular expression to find"`
	Regex         bool   `json:"regex,omitempty" jsonschema:"interpret query as a Go regular expression"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" jsonschema:"perform a case-sensitive search"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema:"maximum matches returned; defaults to 200"`
}

type FileSearchOutput struct {
	Matches []workspace.SearchMatch `json:"matches"`
}

func fileSearch(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, FileSearchInput) (*mcp.CallToolResult, FileSearchOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in FileSearchInput) (*mcp.CallToolResult, FileSearchOutput, error) {
		matches, err := manager.Search(in.Workspace, in.Query, in.Regex, in.CaseSensitive, in.MaxResults)
		return nil, FileSearchOutput{Matches: matches}, err
	}
}

func gitStatus(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, WorkspaceInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in WorkspaceInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
		out, err := manager.Git(ctx, in.Workspace, "status", "--porcelain=v2", "--branch")
		return nil, out, err
	}
}

type GitDiffInput struct {
	Workspace string `json:"workspace" jsonschema:"configured workspace name"`
	Staged    bool   `json:"staged,omitempty" jsonschema:"show staged changes instead of working-tree changes"`
	Path      string `json:"path,omitempty" jsonschema:"optional path relative to the workspace"`
}

func gitDiff(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, GitDiffInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GitDiffInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
		args := []string{"diff"}
		if in.Staged {
			args = append(args, "--cached")
		}
		if in.Path != "" {
			args = append(args, "--", in.Path)
		}
		out, err := manager.Git(ctx, in.Workspace, args...)
		return nil, out, err
	}
}

type GitRemoteInput struct {
	Workspace string `json:"workspace" jsonschema:"configured workspace name"`
	Remote    string `json:"remote,omitempty" jsonschema:"remote name; defaults to origin"`
	Branch    string `json:"branch,omitempty" jsonschema:"optional branch name"`
}

func gitPull(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, GitRemoteInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GitRemoteInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
		remote := in.Remote
		if remote == "" {
			remote = "origin"
		}
		args := []string{"pull", remote}
		if in.Branch != "" {
			args = append(args, in.Branch)
		}
		out, err := manager.Git(ctx, in.Workspace, args...)
		return nil, out, err
	}
}

type GitCommitInput struct {
	Workspace string `json:"workspace" jsonschema:"configured workspace name"`
	Message   string `json:"message" jsonschema:"commit message"`
	AddAll    *bool  `json:"add_all,omitempty" jsonschema:"stage all changes before committing; defaults to true"`
}

func gitCommit(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, GitCommitInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GitCommitInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
		if strings.TrimSpace(in.Message) == "" {
			return nil, workspace.CommandResult{}, errors.New("commit message cannot be empty")
		}
		addAll := in.AddAll == nil || *in.AddAll
		if addAll {
			stage, err := manager.Git(ctx, in.Workspace, "add", "-A")
			if err != nil {
				return nil, stage, err
			}
			if stage.ExitCode != 0 {
				return nil, stage, nil
			}
		}
		out, err := manager.Git(ctx, in.Workspace, "commit", "-m", in.Message)
		return nil, out, err
	}
}

func gitPush(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, GitRemoteInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GitRemoteInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
		remote := in.Remote
		if remote == "" {
			remote = "origin"
		}
		args := []string{"push", remote}
		if in.Branch != "" {
			args = append(args, in.Branch)
		}
		out, err := manager.Git(ctx, in.Workspace, args...)
		return nil, out, err
	}
}

type ExecInput struct {
	Workspace      string            `json:"workspace" jsonschema:"configured workspace name"`
	Command        string            `json:"command" jsonschema:"program name or executable path"`
	Args           []string          `json:"args,omitempty" jsonschema:"program arguments"`
	Environment    map[string]string `json:"environment,omitempty" jsonschema:"additional environment variables for this process"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"execution timeout in seconds; defaults to 300"`
}

func runExec(manager *workspace.Manager) func(context.Context, *mcp.CallToolRequest, ExecInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ExecInput) (*mcp.CallToolResult, workspace.CommandResult, error) {
		out, err := manager.Exec(ctx, in.Workspace, in.Command, in.Args, in.Environment, in.TimeoutSeconds)
		if err != nil {
			return nil, out, fmt.Errorf("execute in workspace: %w", err)
		}
		return nil, out, nil
	}
}
