# Workspace MCP

Workspace MCP connects AI agents to named local workspaces. It exposes focused tools for files and Git, native image input, and one unrestricted CLI execution tool for everything else.

```text
ChatGPT / Codex / MCP client
             │
             │ MCP
             ▼
           wmcp
             │
      ┌──────┼──────┐
      ▼      ▼      ▼
    Files   Git    CLI
             │
             ▼
         Workspace
```

## Tools

| Tool | Purpose | R | D | I | O |
| --- | --- | :-: | :-: | :-: | :-: |
| `workspace_list` | List configured workspaces and capabilities | true | false | true | false |
| `workspace_info` | Inspect one workspace | true | false | true | false |
| `file_tree` | List workspace files | true | false | true | false |
| `file_info` | Inspect file metadata, MIME type, and SHA-256 | true | false | true | false |
| `file_read` | Read UTF-8 text and return its SHA-256 | true | false | true | false |
| `file_write` | Create or replace a UTF-8 text file | false | true | true | false |
| `image_read` | Return PNG, JPEG, GIF, or WebP as MCP image content | true | false | true | false |
| `file_search` | Search text files using text or a regular expression | true | false | true | false |
| `git_status` | Read branch and working-tree status | true | false | true | false |
| `git_diff` | Read working-tree or staged changes | true | false | true | false |
| `git_pull` | Pull with the installed Git CLI | false | true | false | false |
| `git_commit` | Stage and commit changes | false | false | false | false |
| `git_push` | Push with configured Git credentials | false | true | false | false |
| `exec` | Run any local CLI in the workspace | false | true | false | true |

R, D, I, and O are the MCP `readOnlyHint`, `destructiveHint`, `idempotentHint`, and `openWorldHint` annotations. Every tool sets all four values explicitly.

## Requirements

- Go 1.26.2 or newer to build
- Git for Git tools
- Any CLI required by a workspace, such as XeLaTeX, Python, Node.js, Go, MATLAB, or FFmpeg

The OpenAI Tunnel integration uses the newest tunnel-client release compatible with Go 1.26 (`v0.0.12`).

## Build

```powershell
go build -o bin\wmcp.exe .\cmd\wmcp
```

## Configuration

Without a configuration file, the current directory is available as a workspace named `workspace`:

```powershell
wmcp workspace list
wmcp workspace info workspace
wmcp serve
```

For named workspaces, copy `config.example.yaml` to `%USERPROFILE%\.wmcp\config.yaml`:

```yaml
workspaces:
  workspace:
    root: ${WMCP_WORKSPACE_ROOT}
    description: Primary workspace
```

Set `WMCP_WORKSPACE_ROOT`, or replace the placeholder with a path. Select another configuration with `--config PATH` or `WMCP_CONFIG`.

## Local MCP clients

```powershell
wmcp serve
```

```json
{
  "mcpServers": {
    "workspace": {
      "command": "wmcp",
      "args": ["serve"]
    }
  }
}
```

## ChatGPT

Create an OpenAI Secure MCP Tunnel, store its runtime values in environment variables, and start the connection:

```powershell
$env:CONTROL_PLANE_TUNNEL_ID = "tunnel_..."
$env:CONTROL_PLANE_API_KEY = "..."
wmcp tunnel
```

`wmcp tunnel` runs the MCP server and the official OpenAI tunnel client in one process. It opens no local network port.

## Design

Common operations use typed tools with predictable schemas and results. `exec` runs the workspace's existing toolchain for builds, document conversion, media inspection, scripting, and other project-specific work.

Text files use `file_read` and `file_write`. Images use `image_read` so the model receives native visual content. Other formats are handled by an appropriate CLI through `exec`, with text returned on standard output or generated images read through `image_read`.

## License

[MIT](LICENSE)
