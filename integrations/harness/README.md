# DeepSeek Harness provider

This plugin uses Harness's official `LlmAdapter` interface to call Local Runtime's
model API. Harness retains its agent loop, tools, permissions and sessions.
Neither the plugin nor the model endpoint executes requested tools.

Install/copy this package beneath a Harness project with the declared peer
dependencies, then mount its absolute `index.mjs` path in the existing profile:

```yaml
- insert:
    - id: llm-local-runtime
      name: /absolute/path/to/local-runtime-plugin/index.mjs
      config:
        provider: local-runtime
        baseURL: http://127.0.0.1:9317/v1
        apiKeyEnv: LOCAL_RUNTIME_MCP_CLIENT_TOKEN
        models:
          - id: deepseek-web
            name: DeepSeek browser session
```

Disable another adapter that already owns the `local-runtime` provider route.
Supply the named credential through Harness's credentials service or the target
process's launch environment; do not put a key in this file. This is the Local
Runtime model key, not a DeepSeek official API key or a machine-control key.

The generic pi-ai Chat Completions adapter adds its catalog's `maxTokens` even
when a caller omitted it. The browser endpoint cannot honor that control, so this
small provider adapter sends no invented sampling controls or token defaults.
Explicit `temperature`, `maxTokens`, `stop` or `reasoningEffort` is rejected before
network I/O. It advertises text input without unknown capacity or output limits,
and disables automatic replay of model requests.

Text, assistant tool calls and correlated tool results are preserved. Prior
reasoning blocks are not replayed into the text-only source; images and other
content kinds are refused. Real SSE text is emitted incrementally, completed
tool calls become Harness blocks, and errors/truncated streams cannot finish as
successful answers. Breaking consumption cancels the HTTP request, letting the
runtime stop the current turn while retaining its owned conversation. Explicit
`DELETE /v1/conversations/{id}?model=...` releases that conversation's browser tab;
it does not delete the website's saved chat history.

The real Harness `options.sessionId` is sent as `conversation_id`, keeping one
agent conversation on its runtime-owned webpage across tool rounds. Different
Harness sessions use different IDs. A caller with no session ID is refused before
network I/O; the adapter does not invent cross-request identity. This requires a
Local Runtime version that accepts and isolates mandatory `conversation_id`.
Every SSE event must echo that ID; a mismatched stream cannot become an answer.

Interface contracts are tested against official Harness packages `0.2.2-rc.1`;
a real tool-and-answer loop is a separate release acceptance requirement. This follows the supported
[custom-adapter interface](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/user/develop/practice/llm-adapter.md)
without modifying Harness or dependencies.
