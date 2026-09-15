# Local Runtime MCP

Local Runtime MCP gives an AI model controlled access to the runtime where `lrmcp` is running: processes, files, images, the desktop, and the user's Chromium browser. It ships as one Go binary with two entry points over one shared implementation:

- MCP is the primary interface for external AI clients.
- CLI exposes the same operations for setup, scripting, debugging, and progressive discovery.

The name describes the boundary, not the network topology. “Local” means local to the `lrmcp` process. Run it on a laptop, VM, workstation, or remote server and the tools operate there.

## Architecture

```text
AI client ── stdio/OpenAI Tunnel ── MCP tools ─┐
                                               ├── shared capabilities
operator ───────────── CLI commands ───────────┘
                                                    ├─ process
                                                    ├─ filesystem
                                                    ├─ image
                                                    ├─ computer backends
                                                    └─ browser extension bridge
```

Browser control has one implementation path. The bundled Chromium extension uses the browser's `debugger` API (CDP) against existing, logged-in tabs and communicates with `lrmcp` over an authenticated loopback HTTP bridge. There is no second browser launcher or hidden automation profile. The extension assets are embedded in the executable and extracted by a CLI command when needed.

Computer control is deliberately separate from browser control. It captures and operates the complete desktop, while browser tools provide semantic page snapshots, element references, tab operations, JavaScript evaluation, and viewport screenshots.

## Capability domains

### Process

`process_run` launches an executable directly with an argument array—never an implicit shell string. It supports a root-relative working directory, environment overrides, stdin, timeouts, and separate bounded stdout/stderr capture. A nonzero program exit is returned as structured data; failure to start is a tool error.

### Filesystem

All paths are relative to an explicitly configured root. Absolute paths, `..` traversal, and symbolic-link escapes are rejected.

| MCP tool | Purpose |
| --- | --- |
| `filesystem_roots` | List named roots |
| `filesystem_list` | Traverse a directory without following symlinks |
| `filesystem_stat` | Return type, size, MIME type, timestamp, and file digest |
| `filesystem_read_text` | Read bounded UTF-8 text with a complete-file SHA-256 |
| `filesystem_write_text` | Atomically create or replace text, optionally with a SHA guard |
| `filesystem_edit_text` | Atomically perform an exact edit; ambiguous matches fail by default |
| `filesystem_search_text` | Search bounded text files with literal text or a Go regular expression |

Git-specific wrappers are intentionally absent. Git and every other installed CLI are available through `process_run` without duplicating command-line interfaces as MCP tools.

### Image

`image_read` is separate from text reading because images are a different model modality. It validates PNG, JPEG, GIF, or WebP, returns dimensions and a digest, and emits native MCP image content rather than base64 inside a text result.

### Computer

| MCP tool | Purpose |
| --- | --- |
| `computer_screenshot` | Capture the virtual desktop as native PNG content |
| `computer_action` | Move/click/drag, type Unicode text, press a key combination, or scroll |

Backends:

- Windows: native GDI capture and User32 input; no helper process.
- macOS: built-in `screencapture`; input uses the optional `cliclick` command.
- Linux desktop: `gnome-screenshot` or `scrot`; input uses `xdotool` (X11/XWayland).

Desktop input is disabled for MCP unless `computer.enabled` is true. CLI computer commands are explicit operator actions and can be invoked directly.
The Windows capture/input process must run in the signed-in interactive desktop session; Windows intentionally blocks services and isolated window stations from observing or injecting into the user's desktop.

### Browser

| MCP tool | Purpose |
| --- | --- |
| `browser_status` | Report bridge and extension connectivity |
| `browser_tabs` | List controllable tabs |
| `browser_open` / `browser_close` | Create or close tabs |
| `browser_navigate` | Navigate an existing tab |
| `browser_snapshot` | Read visible text and referenced interactive elements |
| `browser_screenshot` | Capture the viewport as native PNG content |
| `browser_action` | Click/type by CSS selector or snapshot ref, press keys, scroll, evaluate JavaScript, back/forward/reload |

The extension requests `debugger`, `tabs`, `scripting`, `downloads`, `storage`, `activeTab`, and all-site host access. Chromium still prevents extensions from controlling protected browser pages and the extension store. The pre-existing ChatGPT browser extension is not used: its host protocol is private to that product and is not a stable integration surface for this server.

## Build

Go 1.27 or newer is required.

```powershell
go test ./...
go build -o .\bin\lrmcp.exe .\cmd\lrmcp
.\bin\lrmcp.exe version
```

The OpenAI Tunnel dependency currently establishes the minimum Go version.

## Configuration

The default path is `%USERPROFILE%\.lrmcp\config.yaml` on Windows or `~/.lrmcp/config.yaml` elsewhere. Set `LRMCP_CONFIG` or pass `--config` to select another file. If the default file is absent, only the current directory is exposed as root `default`, and browser/computer MCP control stays disabled.

```yaml
roots:
  default:
    path: ${LRMCP_ROOT}
    description: Primary filesystem access root

browser:
  enabled: true
  listen: 127.0.0.1:9315
  token: ${LRMCP_BROWSER_TOKEN}

computer:
  enabled: true
```

Generate a browser token and install the embedded extension files:

```powershell
$env:LRMCP_BROWSER_TOKEN = lrmcp browser token
lrmcp browser extension --config C:\path\to\config.yaml
```

Open `edge://extensions` or `chrome://extensions`, enable Developer mode, choose **Load unpacked**, and select the `directory` printed by the command. Supplying `--config` packages the loopback address and token into that local extension copy; its options page can change them later. The bridge refuses non-loopback listen addresses and tokens shorter than 32 characters.

## MCP entry points

For a local stdio client:

```powershell
lrmcp serve --config C:\path\to\config.yaml
```

Example Codex configuration:

```toml
[mcp_servers.local_runtime]
command = "C:\\path\\to\\lrmcp.exe"
args = ["serve", "--config", "C:\\path\\to\\config.yaml"]
```

For a cloud ChatGPT connection, create an OpenAI Tunnel and set the credentials supplied by that workflow:

```powershell
$env:CONTROL_PLANE_TUNNEL_ID = "..."
$env:CONTROL_PLANE_API_KEY = "..."
lrmcp tunnel --config C:\path\to\config.yaml
```

The tunnel and MCP server run in the same process. Only the browser extension bridge opens a local port, bound to loopback and protected by its independent token.

## CLI examples

```powershell
lrmcp filesystem roots
lrmcp filesystem read --root default --path README.md
lrmcp filesystem edit --root default --path README.md --old "old" --new "new"
lrmcp process run --root default --directory . -- go test ./...
lrmcp image read --root default --path screenshot.png --metadata-only
lrmcp computer screenshot --output C:\path\to\desktop.png
lrmcp browser tabs --config C:\path\to\config.yaml
lrmcp browser snapshot --config C:\path\to\config.yaml --tab 123
lrmcp browser action --config C:\path\to\config.yaml --tab 123 --kind click --ref r4
```

Every CLI result is JSON except `help`, `version`, and the raw token generator.

## Security model

- Filesystem and process directories remain inside canonical configured roots, including through symlinks.
- Text mutations are atomic and can use SHA-256 optimistic concurrency checks.
- Process output and execution time are bounded by default.
- Browser traffic is loopback-only and authenticated; browser and desktop MCP control require explicit configuration.
- Tool annotations mark mutations and open-world operations so clients can apply approval policies.
- `process_run`, `browser_action`, and `computer_action` are intentionally powerful. Run the server with the OS account and root set that match the trust granted to the connecting model.

## 2.0 breaking change

Version 2.0 intentionally removes every 1.x identity, configuration shape, capability detector, legacy file command, generic execution alias, and Git wrapper. There are no aliases or compatibility shims. The supported surface is `local-runtime-mcp`, `lrmcp`, `roots`, the domain-prefixed MCP tools, and `process_run`.

## License

[MIT](LICENSE)
