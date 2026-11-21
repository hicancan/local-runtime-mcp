# Browser and local app capabilities

The capability tree is the catalog. Browser capabilities use an owner-authored
JSON definition beside a same-named JavaScript function. A local app capability
uses a JSON definition with a fixed command.

```text
capabilities/
  deepseek/
    start.json
    start.js
    read.json
    read.js
    stop.json
    stop.js
  icourse/
    outline.json
    outline.js
```

`icourse.outline` is a read-only example from a different site: it observes
the loaded courseware outline and explicitly distinguishes unexpanded children
from loaded items. It never starts a video or opens/submits a quiz. Course
identity, schedules and answers do not belong in the runtime core.

A capability must live inside a namespace directory: `deepseek/start.json`
becomes `deepseek.start`. Root-level tool definitions are rejected, so a file
cannot replace built-in tools such as `browser_action`. Names use letters,
digits, `_`, `-`, and namespace dots, with a maximum of 128 characters.

## Definition

```json
{
  "title": "Read a page title",
  "description": "Read the title from a selected example page",
  "origins": ["https://example.com"],
  "inputSchema": {
    "type": "object",
    "properties": {},
    "additionalProperties": false
  },
  "outputSchema": {
    "type": "object",
    "properties": {"title": {"type": "string"}},
    "required": ["title"],
    "additionalProperties": false
  },
  "annotations": {
    "readOnlyHint": true,
    "destructiveHint": false,
    "idempotentHint": true,
    "openWorldHint": true
  }
}
```

The filename supplies the name; do not repeat `name` in the JSON. `description`,
exact HTTP(S) `origins`, object input/output schemas, and all four annotation
booleans are required. Title is optional. Schemas use JSON Schema 2020-12 through
the same validator as the MCP SDK. Local schema references work; remote schemas
are never fetched. Declare actual side effects: sending a message is neither
read-only nor safe to replay.

`origins` restricts the execution target. It does **not** grant a calling website
permission to use the capability. Website access is a separate owner-controlled
grant, and annotations are descriptions rather than authorization.

The same-named `.js` file is a self-contained function expression, not an ES
module. It accepts the validated JSON argument object and returns an object
matching `outputSchema`; it can return a Promise:

```javascript
async function (input) {
  return { title: document.title };
}
```

It runs in the selected page through the existing CDP evaluation channel. There
is no additional JavaScript server or Node dependency at runtime. Treat these
files as trusted local code; the page's contents cannot install capabilities.
Definitions are validated together when loaded. An invalid configured directory
fails rather than exposing a partial catalog.

## Calling a capability

MCP exposes each registered name with the routing envelope:

```json
{
  "browser_id": "the instance returned by browser_status",
  "tab_id": "the opaque tab returned by browser_tabs or browser_open",
  "arguments": {}
}
```

The runtime checks the arguments, verifies that the tab belongs to that browser
and has an allowed origin, checks the origin again inside the page before
running the function, and validates the returned object. Parameters are passed
as JSON values, never interpolated as executable source. A page navigation or
unknown outcome does not automatically repeat an action.

## Same-computer native clients

With `lrmcp serve --transport local` or `lrmcp serve --transport http`, the owner
can configure scoped clients under `grants.clients` in `local-runtime-mcp.yaml`
(or create one with `lrmcp setup client --id studev-win`). Each client holds its
own independent token (or receives `LOCAL_RUNTIME_MCP_CLIENT_TOKEN` /
`LOCAL_RUNTIME_MCP_CLIENT_TOKEN_<CLIENT_ID>` in a single child process) and connects
through the local-only plane at `127.0.0.1:9317` by default. If multiple browser
profiles are connected, set `browser_id` on that client grant. The configured
browser must have exactly one tab open at the exact configured target URL,
including any fragment.

Send `Authorization: Bearer <client token>` on both requests:

- `GET /capabilities` returns that client's granted read-only browser tool definitions.
- `POST /capabilities/invoke` takes `{"name":"icourse.outline","input":{}}`
  as `application/json` and returns the validated tool result as JSON.

The local-only plane rejects browser `Origin` headers and reverse-proxy forwarding
headers, and is never exposed through tunnels. A client token cannot authenticate
machine MCP control (`/mcp`). Errors from the capability API carry an `error`
object with a `code` and `message`.

## Local app tools

Local app tools are exposed on the same-host MCP server (`lrmcp serve --transport stdio`
and the local-only `/mcp` plane on `127.0.0.1:9317`). Public HTTP (`127.0.0.1:9316`),
tunnels, web grants and client tokens never expose local app command bindings.

To expose the read-only StuDev Windows query, add `studev/next-class.json`
under the configured capability directory and point it at your installed
copy of StuDev:

```json
{
  "title": "查下节课",
  "description": "Read the next class from this device's StuDev timetable.",
  "command": {
    "executable": "D:\\Apps\\StuDev\\StuDev.exe",
    "args": ["--next-class"]
  },
  "inputSchema": {"type": "object", "additionalProperties": false},
  "outputSchema": {
    "type": "object",
    "properties": {
      "state": {"enum": ["found", "class_required", "no_upcoming_class", "timetable_unavailable"]},
      "name": {"type": "string"},
      "date": {"type": "string"},
      "starts_at": {"type": "string"},
      "ends_at": {"type": "string"},
      "room": {"type": ["string", "null"]}
    },
    "required": ["state"],
    "additionalProperties": false
  },
  "annotations": {
    "readOnlyHint": true,
    "destructiveHint": false,
    "idempotentHint": true,
    "openWorldHint": false
  }
}
```

The client invokes `studev.next-class` with `{}`. The owner picks the
absolute executable, arguments and optional fixed `command.environment`.
For an isolated StuDev data directory, set `STUDEV_DATA_DIR` in that object.
The runtime starts the command directly. It allows up to five seconds and 64
KiB of stdout, and validates one JSON object before returning it.

Capabilities use the existing short browser-call deadline. Model adapters may
compose `start`, `read`, and `stop` rather than keep one browser evaluation open
through a long response. Cancellation reaches the execution channel; it does not
undo page side effects or prove a website has stopped generating. A stop
capability must observe the page and report whether stopping was confirmed.

## Tools an AI distils

An AI that explores a website with `browser_snapshot` and `browser_action` can
write down how to do one task there with `capability_propose`: a definition and
a page-side function, validated exactly as this catalog validates its files.
The draft waits in `.proposed/` beside the catalog (directories starting with a
dot are never loaded) and runs nothing.

The owner sees the drafts for a site in the extension's panel (click the
toolbar L, under "Offer this site's tools"), can read the code, and approves or
rejects each one. Approval moves the files into the catalog and reloads it;
connected MCP clients receive the new tool list at once. When this directory is
a Git repository, each approval is one commit, so every version of a distilled
tool can be read back and restored. Proposing an approved name again offers a
replacement, for when the site changes and the old tool stops working.

`capability_propose` is listed only when a capability directory is configured.
Only the extension, holding the bridge credential, can approve; the AI that
proposed cannot.
