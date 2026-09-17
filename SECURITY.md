# Security Policy

Local Runtime MCP is a host-access runtime. It gives an authenticated MCP client the ability to run programs, access files, observe and control the interactive desktop, and use browser sessions with the permissions of the account running `lrmcp`. Treat access to the runtime as privileged access to that account.

## Supported versions

Security fixes are released on the latest version. Upgrade to the current [GitHub release](https://github.com/hicancan/local-runtime-mcp/releases/latest) before reporting or validating a security issue.

## Security model

The runtime keeps connection delivery separate from machine capabilities, but every connection reaches the same capability server.

| Boundary | Security properties |
| --- | --- |
| stdio | Inherits the trust boundary of the parent process and its operating-system account. |
| Streamable HTTP | Listens on loopback only, requires a Bearer token of at least 32 characters, validates the accepted host, rejects browser-origin requests, and compares tokens in constant time. |
| OpenAI Tunnel | Uses the configured tunnel ID and API key to carry MCP traffic through OpenAI Secure MCP Tunnel. |
| Cloudflare Tunnel | Keeps the MCP HTTP origin on loopback, requires the MCP Bearer token, and runs the `cloudflared` companion with a temporary token file and a sanitized environment. |
| Browser bridge | Listens on loopback, requires a token of at least 32 characters, accepts one extension instance at a time, and rejects mismatched extension versions. |

The capability domains operate with the authority of the current user:

- **Process** can start installed programs, provide environment values and input, and retain bounded output for active sessions.
- **Filesystem** accepts direct machine paths and can read, create, replace, and patch files. Symlinks are reported but are not followed for mutation.
- **Image** decodes supported local image files and can return their pixels to the MCP client.
- **Computer** can capture the Windows desktop, inspect UI Automation data, activate windows, and generate real keyboard, pointer, and scroll input.
- **Browser** uses the Chromium debugger API and can inspect and operate tabs in the browser profile where the extension is loaded, including authenticated sessions.

The runtime does not add a separate permission sandbox between an authenticated client and these capabilities. Operating-system account boundaries, browser profile boundaries, and the selected MCP client's authorization model remain the effective security boundaries.

## Deployment guidance

- Run `lrmcp` as a dedicated, non-administrator account with access only to the resources the client needs.
- Protect the portable runtime directory. `local-runtime-mcp.yaml` can contain tunnel credentials and Bearer tokens, and the generated browser extension configuration contains its loopback bridge token.
- Generate unique tokens for each runtime directory, keep them out of source control and logs, and rotate them after disclosure or when a client should lose access.
- Prefer stdio for a local client. For remote access, use one supported tunnel provider and keep the HTTP listener on its enforced loopback address.
- Restrict tunnel and provider accounts with the narrowest practical permissions. Review the provider's access logs and security controls independently.
- Load the browser extension only in the browser profile intended for agent access. That profile's open tabs, page content, and authenticated sessions may be reachable through the Browser domain.
- Stop `lrmcp` when the machine should be offline. Runtime shutdown closes the selected connection, terminates owned process sessions, closes the browser bridge, and removes temporary helper files.
- Download binaries from the official GitHub release and verify the SHA-256 digest published with the release asset.

## Reporting a vulnerability

Please report vulnerabilities privately through [GitHub private vulnerability reporting](https://github.com/hicancan/local-runtime-mcp/security/advisories/new). If that channel is unavailable, email [mail@hicancan.top](mailto:mail@hicancan.top).

Include the affected version, operating system, connection mode, reproduction steps, security impact, and any proposed mitigation. Remove credentials, personal files, screenshots, and unrelated machine data before attaching evidence. Do not open a public issue for an undisclosed vulnerability.

We aim to acknowledge a complete report within seven days and will coordinate validation, remediation, release, and disclosure with the reporter.
