# Browser capabilities

Each capability is an owner-authored JSON definition beside a same-named
JavaScript function. The directory is the catalog; no registration manifest is
needed.

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

Capabilities use the existing short browser-call deadline. Model adapters may
compose `start`, `read`, and `stop` rather than keep one browser evaluation open
through a long response. Cancellation reaches the execution channel; it does not
undo page side effects or prove a website has stopped generating. A stop
capability must observe the page and report whether stopping was confirmed.
