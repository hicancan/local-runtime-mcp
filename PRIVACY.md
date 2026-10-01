# Privacy

Effective date: October 1, 2026

Local Runtime MCP is self-hosted software. Capability data is processed on the machine running `lrmcp` and returned through the selected MCP connection. The project operates no runtime hosting, analytics, telemetry collection, or storage service that receives tool data.

## Capability data

| Domain | Data processed | Results sent to the MCP client |
| --- | --- | --- |
| Process | Program paths, arguments, directories, environment overrides, stdin, stdout, and stderr | Session identity, status, and bounded output |
| Filesystem | Paths, directory entries, metadata, text, hashes, searches, and requested mutations | Requested text and metadata, search results, and commit results |
| Image | Local PNG, JPEG, GIF, and WebP data | Native image content and metadata, optionally cropped or resized |
| Computer | Window identity and titles, WGC frames, UIA elements, task labels, input coordinates, and control/observation identities | Control occupancy, pixels, UI state, and action results |
| Browser | Extension instance IDs and labels, tab titles and URLs, page text, accessibility references, screenshots, upload paths, script results, and requested actions | Named instances, tab identities, observations, images, and action results |

Process-launched programs and browser pages may independently read, write, or transmit data according to their own code, permissions, and settings. Separate browser profiles can still access the same remote account or shared document.

## Connection data flows

- **stdio:** local pipes between the MCP client and the runtime.
- **Streamable HTTP:** the authenticated loopback MCP endpoint, optionally reached through an operator-configured reverse proxy.
- **OpenAI Tunnel:** OpenAI tunnel infrastructure, the official HTTP forwarding client, and a private loopback MCP endpoint created for this runtime invocation.
- **Cloudflare Tunnel:** Cloudflare infrastructure and the official `cloudflared` companion forwarding authenticated MCP HTTP traffic to loopback.

Browser extensions connect to the configured loopback bridge using an authenticated WebSocket for commands and periodic keepalive messages, and authenticated HTTP for results and desktop-operation authorization. The bridge routes each request to its selected profile instance and tab. Page results travel through the active MCP connection like other tool results.

External MCP clients and tunnel providers may process request content and connection metadata under their own terms and privacy policies. Their histories, logs, retention, and account data are governed by those services and the operator's configuration.

## Local storage and retention

| Location | State and lifetime |
| --- | --- |
| `local-runtime-mcp.yaml` beside the executable | Persistent connection settings and browser, HTTP, OpenAI, or Cloudflare credentials selected by the operator. |
| Unpacked extension directory | Packaged loopback bridge address and token written by browser setup. |
| Each profile's extension storage | Bridge settings, readable profile label, and stable extension instance UUID. The operator can remove them by resetting or uninstalling that profile's extension. |
| Process memory | Bounded output and active session state; completed, uncollected sessions expire after ten minutes. Host shutdown cleans up owned processes. |
| Bridge and extension memory | Bounded pending commands, connection generations, tab handles, page epochs, retained references, and screenshot identities. Extension restart invalidates prior observation handles. |
| Computer memory | Control token, task label, expiry, actionable state IDs, UIA references, and current operation state. Release, idle expiry, local Stop, and shutdown clear actionable control state. |
| Runtime-only OpenAI state | Private loopback port and freshly generated private-hop credential held in memory, discarded with that invocation. |
| Temporary helper directories | Extracted Windows native worker and the Cloudflare token file, removed on normal component shutdown. |
| Machine files and browser data | Requested file changes, downloaded content, browser history, and program-produced artifacts persist according to their owning application and operator settings. |

The desktop overlay displays the current task label and AI interaction marker locally. Its Stop control revokes desktop input ownership. The overlay is excluded from feedback capture; captured application pixels may still contain sensitive information visible to the host account.

The core runtime retains operational state rather than a persistent tool-activity history. Terminal diagnostics, provider logs, OS logs, browser history, MCP client conversation history, and invoked program data have their own retention policies. Unexpected crashes or forced termination can leave temporary files for the operator to remove.

## Operator choices

Choose which client and provider connect, which OS account runs the runtime, and which browser profiles load the extension. Protect the portable runtime directory and its configuration when moving it between machines. Browser profile identity and stored sessions remain with the browser profile on the machine where it is installed.

Use dedicated accounts and profiles when workload separation is appropriate. Explicit browser IDs, tab handles, and desktop control tokens help route work inside the shared runtime; authenticated clients continue to share the host account's authority.

## Questions and updates

Privacy-relevant changes are recorded in this policy and the public Git history. Send questions to [mail@hicancan.top](mailto:mail@hicancan.top). Follow [SECURITY.md](SECURITY.md) for vulnerabilities.
