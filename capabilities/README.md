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

With `lrmcp serve --transport http`, the owner can enable `capabilities.native`
in `local-runtime-mcp.yaml`. It reuses the fixed capability targets of the
web grant named by `origin`. Use a separate `key` or supply it to the process
with `LOCAL_RUNTIME_MCP_NATIVE_CAPABILITY_KEY`. Connect through
`127.0.0.1:9317` by default. If multiple browser profiles are connected, set
`browser_id` in this configuration. The configured browser must have exactly
one tab open at the exact configured target URL, including any fragment.

Send `Authorization: Bearer <native capability key>` on both requests:

- `GET /capabilities` returns the granted read-only browser tool definitions.
- `POST /capabilities/invoke` takes `{"name":"icourse.outline","input":{}}`
  as `application/json` and returns the validated tool result as JSON.

The dedicated listener serves no model, machine or tunnel routes. Errors from
the capability API carry an `error` object with a `code` and `message`.

## Local app tools

Local app tools are opt-in on `lrmcp serve --transport stdio`. This catalog
is visible to the same-host MCP client that starts the runtime. HTTP, tunnels,
web grants and model sources publish the browser catalog separately.

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
