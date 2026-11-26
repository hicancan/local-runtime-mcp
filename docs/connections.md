# Connections and OAuth

**English** · [简体中文](connections.zh-CN.md)

One `lrmcp` process owns one Runtime Host. Its MCP entry points publish the same 23 built-in tools (including `agent_report`) plus owner-registered capabilities, and share process sessions, browser targets, filesystem coordination, desktop control, and the embedded SQLite WAL + FTS5 audit log (`lrmcp ui` / `lrmcp audit`).

## Select transport and remote wires

```powershell
# Local MCP clients (auto-starts detached daemon when local.token is set)
./lrmcp.exe serve --transport stdio

# Background daemon lifecycle & status
./lrmcp.exe start
./lrmcp.exe status
./lrmcp.exe stop

# Hot-plug remote wires live onto the running daemon (0 restart!)
./lrmcp.exe tunnel up openai --ttl 2h
./lrmcp.exe tunnel up cloudflare
./lrmcp.exe tunnel up http
./lrmcp.exe tunnel down openai
./lrmcp.exe tunnel down all

# Live per-wire remote boundaries (broadcasts tools/list_changed)
./lrmcp.exe grant remote openai --read-only
```

Remote wires are hot-pluggable and independent. Provider credentials saved in `local-runtime-mcp.yaml` stay idle until that wire is attached (via CLI or the browser extension `L` panel). An OpenAI wire creates its private loopback HTTP origin; `http` and `cloudflare` use the configured public-facing loopback origin (`127.0.0.1:9316`). Both remote wires and all local `stdio` clients share one Host.

Each wire owns its connection lifetime. A wire's failure is recorded in `lrmcp status` while local clients and surviving wires continue unaffected. `lrmcp stop` (or `Ctrl+C` in foreground `lrmcp daemon`) gracefully detaches every wire and shuts down the Host. Recovery restores delivery; retrying a machine action remains a deliberate client decision.

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
6. Hot-plug Cloudflare alone or both remote wires onto the running daemon:

   ```powershell
   ./lrmcp.exe tunnel up cloudflare
   # optional: ./lrmcp.exe tunnel up openai
   ```

7. Continue the client's connection flow. On Local Runtime's consent page, verify the public hostname, registered client, and requested access, then approve using the owner approval key. The browser returns through the registered callback to the client application; token exchange and tool discovery complete the connection.
8. Confirm the client discovers the 23 built-in tools and any capabilities enabled by the owner. Start with read-only version, browser status, and file metadata checks before testing side effects.

### Multi-client cloud access alongside OAuth

When multiple remote workers share the same Cloudflare Tunnel (`https://<public_host>/mcp`), mint one or more named Bearer tokens under `grants.remote.tokens` (which coexist cleanly with `http.auth: oauth` while preserving strict OAuth isolation against legacy `http.bearer_token`):

```powershell
./lrmcp.exe setup remote-token --id worker-1
./lrmcp.exe setup remote-token --id worker-ro --read-only
```

Remote callers send `Authorization: Bearer <token>` and may optionally attach `X-LRMCP-Agent: <sub-id>` so every concurrent worker gets its own live timeline in `lrmcp ui` and `lrmcp audit`.

The callback URI must match exactly, including its path and any query component. Repeat `--redirect-uri` to register multiple permitted callbacks for one named client. Running setup again for the same name adds its callbacks while keeping existing credentials. The client name is its client ID; the default is `gemini`. Registration and `--show` display that client's credentials locally.

Local protocol tests validate the OAuth implementation. Acceptance in Gemini additionally depends on its current host integration, the account's access to custom apps, the copied callback, and public routing; confirm those conditions in the actual client.

## Authorization lifecycle

The public origin supports a selected Bearer or OAuth mode. OAuth uses Authorization Code flow, S256 PKCE, exact callback matching, owner approval, and rotating refresh tokens. OpenAI's private-hop credential is generated for the running process and stays independent of public authentication.

OAuth grants access to the full machine-control surface with the permissions of the account running `lrmcp`. Resource IDs and desktop ownership coordinate work inside that shared authority. For separate security boundaries, run separate OS accounts or machines.

Authorization codes and pending consent tickets remain in memory only. Active OAuth access and refresh grant families are persisted atomically alongside the configuration as SHA-256 digests (`lrmcp-oauth-grants.json`, mode `0600`, zero plaintext tokens on disk) with a sliding 30-day inactivity window, so binary upgrades, daemon restarts, and tunnel reconnects never force clients to re-authorize. Rotating `owner_token`, rotating a client's `client_secret`, or deleting `lrmcp-oauth-grants.json` immediately invalidates the affected persisted grants. Registered client credentials and the owner approval key remain in the portable YAML.

## Cross-provider acceptance

Use one client connected through OpenAI and another through Cloudflare to test the shared Host. Run individual tool calls and record each result and time. Keep credentials, control tokens, screenshots, and private page contents out of the report.

| Check | Evidence |
| --- | --- |
| Same runtime | Both clients obtain the same executable version and tool catalog. |
| Shared process registry | Client A starts a short asynchronous process with a distinctive marker and shares only its `session_id` with B. B reads it with `process_continue` using an explicit cursor; A can still reread the retained output. |
| Shared browser registry | Both clients see the same connected browser instance IDs and can select distinct instances or existing tabs explicitly. |
| Shared desktop coordination | A acquires control with a test label. While A holds it, B's acquire reports busy with A's label. A releases only its own control token. |
| Independent process work | The [two-conversation prompts](concurrency-acceptance.zh-CN.md) produce overlapping process time windows with distinct markers and complete cursor-collected output. |
| Native image result | Compare image blocks and structured metadata separately; an image MIME or dimensions alone do not establish that the host delivered pixels to its model. |

The cross-client process read demonstrates a shared registry. Tenant isolation requires a separate OS account or machine. Each test client cleans up only the process or desktop control it acquired. A missing browser profile or inaccessible image path is reported as uncovered.

## Multiple machines

Run a separate Host on each machine and give each a distinct connection identity and public address. Each machine owns its own process sessions, browser profiles, and interactive desktop. Cloudflare replicas of one tunnel serve interchangeable origins; use distinct tunnels or explicit routes when origins represent different machines with independent resource IDs.
