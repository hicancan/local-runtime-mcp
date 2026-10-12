# Connections and OAuth

**English** · [简体中文](connections.zh-CN.md)

One `lrmcp` process owns one Runtime Host. Its entry points publish the same 21 tools and share process sessions, browser targets, filesystem coordination, and desktop control.

## Select transport and providers

```powershell
# Local process integration
./lrmcp.exe serve --transport stdio

# Direct loopback HTTP
./lrmcp.exe serve --transport http

# One remote provider
./lrmcp.exe serve --transport http --tunnel openai
./lrmcp.exe serve --transport http --tunnel cloudflare

# Both remote providers, one Host
./lrmcp.exe serve --transport http --tunnel openai --tunnel cloudflare
```

The transport is explicit. Tunnels forward HTTP. Provider credentials saved in `local-runtime-mcp.yaml` take effect when that provider is selected. An OpenAI-only invocation creates its private HTTP origin; a direct HTTP or Cloudflare invocation uses the configured public-facing origin. A dual-provider invocation creates both origins against the same MCP Server.

Each provider owns its connection lifetime. A provider's terminal error is reported while a surviving entry point continues serving. `Ctrl+C` stops the complete runtime. If every entry point ends, the Host shuts down and reports the resulting failures. Recovery can restore delivery; retrying a machine action remains a deliberate client decision.

## Credential roles

| Credential | Used by | Authority |
| --- | --- | --- |
| OpenAI tunnel ID and API key | OpenAI forwarding client | Connect the configured OpenAI tunnel |
| Cloudflare tunnel token | `cloudflared` | Connect the configured Cloudflare tunnel |
| HTTP Bearer token | MCP client in Bearer mode | Call the full runtime tool surface |
| OAuth client ID and secret | Registered MCP client's OAuth flow | Identify and authenticate that client |
| Owner approval key | Machine operator on the consent page | Approve the requested runtime access |
| OAuth access and refresh tokens | Authorized MCP client | Call or renew access to the runtime |
| Browser bridge token | Installed Chromium extension | Connect that profile to the local bridge |

These values are stored beside the executable where persistent storage is needed. The setup commands generate OAuth credentials locally. Keep the configuration, generated extension settings, terminal output, and removable media private.

## Gemini setup

1. Configure a Cloudflare managed tunnel and a public hostname such as `mcp.example.com`. Route the entire hostname to the loopback origin, normally `http://127.0.0.1:9316`, so the MCP path and OAuth discovery, consent, and token paths are reachable.
2. Set `http.public_host` and `cloudflare.tunnel_token` in the executable's `local-runtime-mcp.yaml`.
3. In Gemini's custom MCP connection dialog, enter `https://mcp.example.com/mcp`, expand the additional settings, and copy the redirect URI supplied by Gemini.
4. Register that exact URI locally:

   ```powershell
   ./lrmcp.exe setup oauth --client-name gemini --redirect-uri "https://<copied-callback-host>/<copied-callback-path>"
   ./lrmcp.exe setup oauth --client-name gemini --show
   ```

5. Enter the generated client ID and client secret in Gemini's OAuth fields. Retain the owner approval key for the consent step. Setup selects `http.auth: oauth`; credentials remain in the adjacent configuration.
6. Start Cloudflare alone or both providers:

   ```powershell
   ./lrmcp.exe serve --transport http --tunnel openai --tunnel cloudflare
   ```

7. Continue the client's connection flow. On Local Runtime's consent page, verify the public hostname, registered client, and requested access, then approve using the owner approval key.
8. Confirm the client discovers 21 tools. Start with read-only version, browser status, and file metadata checks before testing side effects.

The callback URI must match exactly, including its path and any query component. Repeat `--redirect-uri` to register multiple permitted callbacks for one named client. Running setup again for the same name adds its callbacks while keeping existing credentials. The client name is its client ID; the default is `gemini`. Registration and `--show` display that client's credentials locally.

Local protocol tests validate the OAuth implementation. Acceptance in Gemini additionally depends on its current host integration, the account's access to custom apps, the copied callback, and public routing; confirm those conditions in the actual client.

## Authorization lifecycle

The public origin supports a selected Bearer or OAuth mode. OAuth uses Authorization Code flow, S256 PKCE, exact callback matching, owner approval, and rotating refresh tokens. OpenAI's private-hop credential is generated for the running process and stays independent of public authentication.

OAuth grants access to the full machine-control surface with the permissions of the account running `lrmcp`. Resource IDs and desktop ownership coordinate work inside that shared authority. For separate security boundaries, run separate OS accounts or machines.

Authorization codes, pending consent, access tokens, and refresh tokens are in memory. Restarting the runtime requires clients to authorize again. Registered client credentials and the owner approval key remain in the portable YAML. An archive or runtime update preserves the operator's configuration; switching authentication is an explicit setup or configuration action.

## Cross-provider acceptance

Use one client connected through OpenAI and another through Cloudflare to test the shared Host. Run individual tool calls and record each result and time. Keep credentials, control tokens, screenshots, and private page contents out of the report.

| Check | Evidence |
| --- | --- |
| Same runtime | Both clients obtain the same executable version and discover 21 tools. |
| Shared process registry | Client A starts a short asynchronous process with a distinctive marker and shares only its `session_id` with B. B reads it with `process_continue` using an explicit cursor; A can still reread the retained output. |
| Shared browser registry | Both clients see the same connected browser instance IDs and can select distinct instances or existing tabs explicitly. |
| Shared desktop coordination | A acquires control with a test label. While A holds it, B's acquire reports busy with A's label. A releases only its own control token. |
| Independent process work | The [two-conversation prompts](concurrency-acceptance.zh-CN.md) produce overlapping process time windows with distinct markers and complete cursor-collected output. |
| Native image result | Compare image blocks and structured metadata separately; an image MIME or dimensions alone do not establish that the host delivered pixels to its model. |

The cross-client process read demonstrates a shared registry. Tenant isolation requires a separate OS account or machine. Each test client cleans up only the process or desktop control it acquired. A missing browser profile or inaccessible image path is reported as uncovered.

## Multiple machines

Run a separate Host on each machine and give each a distinct connection identity and public address. Each machine owns its own process sessions, browser profiles, and interactive desktop. Cloudflare replicas of one tunnel serve interchangeable origins; use distinct tunnels or explicit routes when origins represent different machines with independent resource IDs.
