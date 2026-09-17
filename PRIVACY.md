# Privacy

Effective date: September 17, 2026

Local Runtime MCP is self-hosted software. The project does not operate a hosted runtime, analytics service, telemetry endpoint, or storage service that receives data from `lrmcp`. Data is processed on the machine running the software and is returned to the MCP client only when a client invokes a capability.

## Data processed by each capability

| Capability | Data it may process | Result returned to the MCP client |
| --- | --- | --- |
| Process | Program paths, arguments, working directories, environment overrides, stdin, stdout, and stderr | Process status and bounded output |
| Filesystem | Direct paths, directory entries, file metadata, text content, hashes, searches, and requested writes or patches | Requested metadata, text, search results, and mutation results |
| Image | Supported local PNG, JPEG, GIF, and WebP files | Image metadata and native image content, optionally cropped or resized |
| Computer | Window metadata, Windows Graphics Capture frames, UI Automation elements, coordinates, and requested input actions | Desktop or window screenshots, semantic UI state, and action results |
| Browser | Tab titles and URLs, accessibility content, page screenshots, evaluated script results, selected upload paths, and requested browser actions | Browser state, page content, screenshots, and action results |

The Process domain can run programs that independently read data, write data, or communicate over a network. Those programs' behavior is governed by their own code and configuration.

## Where data travels

Every invocation uses one selected MCP connection:

- **stdio** carries requests and results through local process pipes.
- **Streamable HTTP** carries requests and results through an authenticated loopback HTTP endpoint.
- **OpenAI Tunnel** carries MCP traffic through OpenAI's tunnel infrastructure.
- **Cloudflare Tunnel** carries authenticated Streamable HTTP traffic through Cloudflare's tunnel infrastructure to the loopback origin.

The Chromium extension communicates only with the configured loopback browser bridge. Browser observations and actions then travel back through the active MCP connection like other tool results.

When a tunnel or external MCP client is used, the corresponding provider may process connection metadata and MCP traffic under its own terms and privacy policy. Local Runtime MCP does not control provider-side logging, retention, or account data.

## Local storage and retention

The runtime stores only the state needed to operate:

- `local-runtime-mcp.yaml`, beside the executable, persists the configuration selected by the operator and may contain browser, HTTP, OpenAI, and Cloudflare credentials.
- `lrmcp setup browser` writes the browser bridge address and token into the unpacked extension directory. The extension can also store its address, token, and generated instance identifier in the Chromium profile's local extension storage.
- Active process sessions retain bounded stdout and stderr in memory. Completed, uncollected sessions expire after ten minutes; shutdown terminates owned sessions.
- Browser commands, retained element references, screenshot identifiers, and Computer state identifiers are held in memory and bounded by the runtime or extension. Native screenshot and image bytes are returned to the requesting MCP client.
- The Windows Computer worker and Cloudflare token file use temporary directories that are removed when their owning runtime component stops normally.
- Files created or changed through Filesystem or Process are intentional machine-side effects and remain until the operator, client, or invoked program removes them.

The runtime does not create an activity history, upload telemetry, or maintain a project-operated copy of tool requests and results. Terminal output from `lrmcp`, provider logs, operating-system logs, browser history, MCP client history, and data created by invoked programs are outside this runtime's retention behavior.

## Operator choices

Operators determine which client connects, which transport is active, which browser profile loads the extension, which account runs the process, what paths and programs are exposed, and which third-party tunnel providers are used. Use a dedicated operating-system account and browser profile when stronger separation from personal data is desired.

## Questions and changes

Privacy-relevant changes are documented in this file and released through the project's public Git history. Questions can be sent to [mail@hicancan.top](mailto:mail@hicancan.top). Security vulnerabilities should follow [SECURITY.md](SECURITY.md).
