# Security Policy

Local Runtime MCP gives authenticated clients access to programs, files, images, browser profiles, and the interactive Windows desktop with the authority of the account running `lrmcp`. Treat connection credentials as privileged access to that account.

## Supported versions

Security fixes are released on the latest version. Use the current [GitHub release](https://github.com/hicancan/local-runtime-mcp/releases/latest) when reporting or validating a vulnerability.

## Connection boundaries

Every selected connection reaches one shared Runtime Host and the same 21-tool surface.

| Connection | Properties |
| --- | --- |
| stdio | Uses local parent-process pipes and the operating-system account boundary. |
| Streamable HTTP | Listens on loopback, requires a Bearer token of at least 32 characters, validates the accepted host, rejects nonempty browser Origin headers, and compares tokens in constant time. |
| OpenAI Tunnel | Authenticates using the configured tunnel ID and API key. Official HTTP forwarding reaches a private loopback endpoint with a dynamically assigned port and a fresh in-memory private-hop credential. |
| Cloudflare Tunnel | The official `cloudflared` companion reaches the configured loopback HTTP origin. The tunnel token authenticates `cloudflared`; the separate MCP Bearer token authenticates tool clients. The companion uses a temporary token file and a sanitized environment. |
| Browser bridge | Listens on loopback, verifies its bridge token, checks the extension version, and routes commands and results to matching instance and connection-generation identities. Multiple configured profile instances may connect. |

The project supports static Bearer authentication for HTTP and provider credentials for tunnel operation. Each credential has a distinct role and should be generated, protected, and rotated independently.

The OpenAI provider-to-Host hop authenticates its runtime-only credential in `X-Local-Runtime-MCP-Internal-Token`. This credential is separate from the external client's forwarded Authorization and Origin metadata. Public `serve http` and Cloudflare origins continue to require the configured Bearer token and reject browser-origin requests. The private-hop credential is generated automatically and adds no operator configuration.

## Resource coordination

The five domains share the host account's permissions:

- **Process** starts programs with explicit arguments, directories, environment overrides, and input. Programs may access other files, devices, and networks with that account's authority.
- **Filesystem** accepts direct machine paths. Version-checked writes and patches coordinate through Host-local path locks; final-component symbolic links are rejected for mutation. External programs participate according to their own filesystem behavior.
- **Image** reads local image data and returns native image content. Encoded bytes, dimensions, pixels, transformation capacity, and output encoding are bounded.
- **Computer** captures windows and desktop pixels, reads UI Automation information, and injects real input. A control token coordinates exclusive desktop input ownership; actionable state IDs and UIA references bind actions to observations.
- **Browser** inspects and operates tabs through the debugger API in connected profiles, including authenticated pages, form input, file upload, dialogs, and evaluated JavaScript.

Resource handles coordinate targets and workflows. All clients authenticated to the runtime retain the same host-access authority. Desktop control tokens allocate shared input rather than an independent desktop; browser profiles provide browser-data separation while their tabs share profile sessions. For stronger workload separation, use dedicated OS accounts, profiles, or isolated machines.

Browser operations affecting a visible page coordinate with desktop ownership. External user input, application events, arbitrary invoked programs, and remote account changes remain participants in the environment. A canceled or failed side-effecting call can have already changed its target; observe before retrying.

## Deployment guidance

- Run under a dedicated, non-administrator account with the permissions appropriate for the intended workloads.
- Protect `local-runtime-mcp.yaml`, generated extension configuration, and removable runtime directories as credential-bearing storage. Restrict file access to trusted operators.
- Generate long random tokens, keep them out of source control and diagnostic evidence, and rotate them after disclosure or access changes.
- Use stdio for local process integrations or the enforced loopback HTTP origin for proxy integrations. Choose one supported tunnel provider for remote reachability.
- Apply the provider account's access controls and review its logging and retention independently.
- Load the extension in each intended profile and give it a distinct label. Select the returned browser and tab IDs explicitly; rediscover targets after extension restarts.
- Keep desktop control tokens private to the workflow that acquired them. Release promptly; use the visible local Stop affordance when interaction should end. The default idle expiry is five minutes.
- Stop `lrmcp` when the machine should be offline. Normal shutdown cancels work, cleans up owned process sessions and injected input, closes connections, and removes temporary helpers.
- Download from official GitHub releases and verify the published SHA-256 values.

## Reporting a vulnerability

Report vulnerabilities privately using [GitHub private vulnerability reporting](https://github.com/hicancan/local-runtime-mcp/security/advisories/new). If that channel is unavailable, email [mail@hicancan.top](mailto:mail@hicancan.top).

Include the version, operating system, connection mode, reproduction steps, impact, and any proposed mitigation. Redact credentials, control tokens, personal files, screenshots, and unrelated machine data. Keep undisclosed vulnerabilities out of public issues.

We aim to acknowledge a complete report within seven days and coordinate validation, remediation, release, and disclosure with the reporter.
