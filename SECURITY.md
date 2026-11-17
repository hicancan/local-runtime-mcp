# Security Policy

Local Runtime MCP gives authenticated clients access to programs, files, images, browser profiles, and the interactive Windows desktop with the authority of the account running `lrmcp`. Treat connection credentials as privileged access to that account.

## Supported versions

Security fixes are released on the latest version. Use the current [GitHub release](https://github.com/hicancan/local-runtime-mcp/releases/latest) when reporting or validating a vulnerability.

## Connection boundaries

Every selected MCP connection reaches one shared Runtime Host and the same built-in and owner-registered tool surface. One process can run OpenAI and Cloudflare providers concurrently; both use the same resource coordination services.

| Connection | Properties |
| --- | --- |
| stdio | Uses local parent-process pipes and the operating-system account boundary. |
| Streamable HTTP | Listens on loopback, validates the accepted host, rejects nonempty browser Origin headers on MCP calls, and uses the selected Bearer or OAuth authorization mode. Static Bearer tokens require at least 32 characters and constant-time comparison. |
| OpenAI Tunnel | Authenticates using the configured tunnel ID and API key. Official HTTP forwarding reaches a private loopback endpoint with a dynamically assigned port and a fresh in-memory private-hop credential. |
| Cloudflare Tunnel | The official `cloudflared` companion reaches the configured loopback HTTP origin. The tunnel token authenticates `cloudflared`; separate Bearer or OAuth credentials authorize tool clients. The companion uses a temporary token file and a sanitized environment. |
| Browser bridge | Listens on loopback, authenticates each WebSocket command connection and HTTP result/authorization request, checks the extension version, and routes commands and results to matching instance and connection-generation identities. Multiple configured profile instances may connect. |

The public HTTP origin supports static Bearer authentication or OAuth authorization. Provider credentials operate tunnels. Each credential has a distinct role and should be generated, protected, and rotated independently.

The OpenAI provider-to-Host hop authenticates its runtime-only credential in `X-Local-Runtime-MCP-Internal-Token`. This credential is separate from the external client's forwarded Authorization and Origin metadata. Direct HTTP and Cloudflare origins require the configured public authorization mode. The private-hop credential is generated automatically and adds no operator configuration.

## OAuth authorization

OAuth uses registered clients, exact callback URI matching, Authorization Code flow with mandatory S256 PKCE, owner-approved consent, and rotating refresh tokens. Discovery identifies the public HTTPS origin configured by `http.public_host`. Register the redirect URI supplied by the intended client with `lrmcp setup oauth`.

The owner approval key confirms that the operator authorizes a client's request. Keep it with trusted operators; supply it only on the runtime's own consent page after checking the public origin and client identity. The generated client ID and secret belong in the requesting client's OAuth settings. Cloudflare tunnel tokens and OpenAI provider keys retain their separate provider roles.

An MCP OAuth grant authorizes the complete tool surface with the host account's permissions. Clients share the runtime's authority and resource managers. Process session IDs, browser handles, and desktop control tokens coordinate resources rather than establishing tenant security boundaries.

## Web and model boundaries

Web SDK callers are separate from full-access MCP clients. The extension supplies the real top-level sender origin, document and browser identity. The Host requires an exact configured origin and per-model or per-capability grant. A web caller cannot supply a target tab, browser identity, script, or runtime credential. Registered capability targets are fixed by the owner; both the Host and execution wrapper verify their allowed origin.

The model API uses a distinct random key of at least 32 characters. It cannot authenticate MCP or the browser bridge, and MCP credentials do not authenticate model requests. Direct model HTTP rejects browser Origin headers; authorized websites use the extension SDK instead. Model routes are not exposed on the private OpenAI MCP forwarding hop.

Owner-authored capability scripts are trusted executable code, not a sandbox for arbitrary websites. Input and output schemas, target-origin checks and safe parameter injection constrain invocation; they do not make an unreviewed owner script trustworthy. Model-produced tool requests are validated and returned to the caller, never executed by the model service.

Client credentials and the owner approval key are stored in the adjacent YAML. OAuth registration and `lrmcp setup oauth --show` deliberately display credentials in the local terminal; keep that output private. OAuth authorization codes, access tokens, refresh tokens, and pending consent state live in process memory; runtime restart invalidates them and requires renewed authorization. Protect configuration backups and removable media containing long-lived credentials.

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
- Use stdio for local process integrations or the enforced loopback HTTP origin for proxy integrations. Enable one or both supported tunnel providers against the same Host for remote reachability.
- Register each OAuth client's exact redirect URI, verify consent on the configured HTTPS origin, and keep the owner approval key private to the machine's operator.
- Apply the provider account's access controls and review its logging and retention independently.
- Load the extension in each intended profile and give it a distinct label. Select the returned browser and tab IDs explicitly; rediscover targets after extension restarts.
- Keep desktop control tokens private to the workflow that acquired them. Release promptly; use the visible local Stop affordance when interaction should end. The default idle expiry is five minutes.
- Stop `lrmcp` when the machine should be offline. Normal shutdown cancels work, cleans up owned process sessions and injected input, closes connections, invalidates in-memory OAuth tokens, and removes temporary helpers. An individual provider exit leaves surviving entry points available; inspect local status when diagnosing partial reachability.
- Download from official GitHub releases and verify the published SHA-256 values.

## Reporting a vulnerability

Report vulnerabilities privately using [GitHub private vulnerability reporting](https://github.com/hicancan/local-runtime-mcp/security/advisories/new). If that channel is unavailable, email [mail@hicancan.top](mailto:mail@hicancan.top).

Include the version, operating system, connection mode, reproduction steps, impact, and any proposed mitigation. Redact credentials, control tokens, personal files, screenshots, and unrelated machine data. Keep undisclosed vulnerabilities out of public issues.

We aim to acknowledge a complete report within seven days and coordinate validation, remediation, release, and disclosure with the reporter.
