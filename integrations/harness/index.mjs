import z from '@deepseek-ai/schemastery';
import { LlmAdapter, LlmError, attributionHeaders, assertUsableApiKey, resolveRetryPolicy } from '@deepseek-ai/dsh-llm';
import { credentialRef } from '@deepseek-ai/dsh-credentials';
import { launchEnvironmentOf } from '@deepseek-ai/dsh-launch-environment';

export const name = 'llm-local-runtime';
export const inject = ['llm'];
export const Config = z.object({
  provider: z.string().default('local-runtime'),
  baseURL: z.string().default('http://127.0.0.1:9316/v1'),
  apiKeyEnv: z.string().role('credential-ref').default('LOCAL_RUNTIME_MCP_MODEL_KEY'),
  models: z.array(z.object({ id: z.string().required(), name: z.string() })).min(1).required(),
});
const noReplay = resolveRetryPolicy({ mode: 'normal', maxRetries: 0 }, 'local-runtime');
const badWire = () => new LlmError('Local Runtime returned an invalid or incomplete model stream.', 'MALFORMED_RESPONSE');

// This is a provider-wire adapter, not an agent loop. It never invokes tools.
export class LocalRuntimeAdapter extends LlmAdapter {
  constructor(config, resolveKey, fetcher = fetch) {
    super();
    let url;
    try { url = new URL(config.baseURL); } catch { throw new LlmError('Configure a valid Local Runtime base URL.', 'INVALID_CONFIG'); }
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new LlmError('Configure a plain HTTP(S) Local Runtime base URL.', 'INVALID_CONFIG');
    this.config = { ...config, baseURL: url.href.replace(/\/$/, ''), models: config.models.map(model => ({ ...model })) };
    this.resolveKey = resolveKey; this.fetcher = fetcher;
  }
  providerInfo(provider) { return { id: provider, name: 'Local Runtime' }; }
  providerRetryPolicy() { return noReplay; }
  async listModels(provider) { return this.config.models.map(model => ({ provider, id: model.id, name: model.name || model.id, inputModalities: ['text'] })); }
  async resolveModel(provider, model) {
    return { provider, id: model, name: this.config.models.find(item => item.id === model)?.name || model, inputModalities: ['text'] };
    // Deliberately no defaultMaxTokens, unknown context capacity, or reasoning controls.
  }
  async *stream(options) {
    for (const field of ['temperature', 'maxTokens', 'stop', 'reasoningEffort']) {
      if (options[field] !== undefined) throw new LlmError('Local Runtime does not expose explicit sampling, token, stop-sequence or reasoning controls.', 'UNSUPPORTED_OPTION');
    }
    if (typeof options.sessionId !== 'string' || !options.sessionId) throw new LlmError('This model route requires a stable Harness sessionId.', 'INVALID_ARGS');
    const controller = new AbortController();
    const signal = options.signal ? AbortSignal.any([options.signal, controller.signal]) : controller.signal;
    let key;
    try { key = assertUsableApiKey(await this.resolveKey(), name, this.config.apiKeyEnv); }
    catch { throw new LlmError('Configure the Local Runtime model credential reference.', 'MISSING_CREDENTIAL'); }
    let response;
    try {
      response = await this.fetcher(`${this.config.baseURL}/chat/completions`, {
        method: 'POST', signal,
        headers: { ...attributionHeaders(), Authorization: `Bearer ${key}`, 'Content-Type': 'application/json', Accept: 'text/event-stream' },
        body: JSON.stringify({ model: options.model, conversation_id: options.sessionId, messages: wireMessages(options), tools: options.tools?.map(tool => ({ type: 'function', function: { name: tool.name, description: tool.description, parameters: tool.parameters } })), stream: true }),
      });
      if (!response.ok) throw new LlmError(`Local Runtime returned HTTP ${response.status}.`, response.status === 401 ? 'AUTH' : response.status === 429 ? 'RATE_LIMIT' : 'SERVER');
      if (!response.body || !response.headers.get('content-type')?.toLowerCase().includes('text/event-stream')) throw badWire();
      let text = '', textStarted = false, finish, done = false;
      const calls = new Map();
      for await (const event of sse(response.body)) {
        if (event === '[DONE]') { done = true; break; }
        let value;
        try { value = JSON.parse(event); } catch { throw badWire(); }
        if (value.conversation_id !== options.sessionId) throw badWire();
        if (value.error) {
          if (value.error.code === 'provider_unavailable') throw new LlmError('The model website is temporarily unavailable; the request was not replayed.', 'SERVER');
          if (value.error.code === 'browser_login_required') throw new LlmError('Sign in to the model website in the selected browser profile.', 'AUTH');
          if (value.error.code === 'model_timeout') throw new LlmError('The model request timed out; source stop was requested.', 'TIMEOUT');
          throw new LlmError('The Local Runtime model source failed; the request was not replayed.', 'SERVER');
        }
        if (!Array.isArray(value.choices) || value.choices.length !== 1 || value.choices[0].index !== 0) throw badWire();
        const choice = value.choices[0], delta = choice.delta || {};
        if (finish !== undefined) throw badWire();
        if (delta.content !== undefined && typeof delta.content !== 'string') throw badWire();
        if (delta.content) {
          if (!textStarted) { textStarted = true; yield { type: 'block-start', index: 0, blockType: 'text' }; }
          text += delta.content; yield { type: 'text-delta', index: 0, text: delta.content };
        }
        for (const fragment of delta.tool_calls || []) {
          if (!Number.isInteger(fragment.index) || fragment.index < 0 || fragment.index > 127) throw badWire();
          const call = calls.get(fragment.index) || { id: '', name: '', arguments: '' };
          if (fragment.id) { if (call.id && call.id !== fragment.id) throw badWire(); call.id = fragment.id; }
          if (fragment.type && fragment.type !== 'function') throw badWire();
          call.name += fragment.function?.name || ''; call.arguments += fragment.function?.arguments || '';
          calls.set(fragment.index, call);
        }
        if (choice.finish_reason !== null && choice.finish_reason !== undefined) finish = choice.finish_reason;
      }
      if (!done || !['stop', 'tool_calls', 'length'].includes(finish) || (calls.size > 0) !== (finish === 'tool_calls')) throw badWire();
      if (textStarted) yield { type: 'block-end', index: 0, block: { type: 'text', text } };
      let index = textStarted ? 1 : 0;
      const allowed = new Set(options.tools?.map(tool => tool.name));
      const ids = new Set();
      for (const [, call] of [...calls].sort(([a], [b]) => a - b)) {
        let args;
        try { args = JSON.parse(call.arguments); } catch { throw badWire(); }
        if (!call.id || ids.has(call.id) || !allowed.has(call.name) || !args || Array.isArray(args) || typeof args !== 'object') throw badWire();
        ids.add(call.id);
        yield { type: 'block-start', index, blockType: 'tool-call' };
        yield { type: 'tool-call-delta', index, id: call.id, name: call.name, argumentsDelta: call.arguments };
        yield { type: 'block-end', index, block: { type: 'tool-call', ...call } }; index++;
      }
      if (!textStarted && !calls.size) throw badWire();
      yield { type: 'finish', reason: { kind: finish === 'tool_calls' ? 'tool-calls' : finish === 'length' ? 'max-tokens' : 'stop' } };
    } catch (error) {
      if (signal.aborted) throw new DOMException('Local model request cancelled.', 'AbortError');
      if (error instanceof LlmError) throw error;
      throw new LlmError('Local Runtime model transport failed; the request was not replayed.', 'TRANSPORT');
    } finally {
      controller.abort();
      await response?.body?.cancel().catch(() => {});
    }
  }
}

function wireMessages(options) {
  const result = options.system === undefined ? [] : [{ role: 'system', content: options.system }];
  for (const message of options.messages) {
    let text = ''; const calls = [];
    for (const block of message.content) {
      if (block.type === 'text') text += block.text;
      else if (block.type === 'tool-call' && message.role === 'assistant') calls.push({ id: block.id, type: 'function', function: { name: block.name, arguments: block.arguments } });
      else if (block.type !== 'reasoning') throw new LlmError('Local Runtime supports text and structured tool history only.', 'UNSUPPORTED_CONTENT');
    }
    if (message.role === 'tool' && message.isError) text = `Tool failed:\n${text}`;
    result.push({ role: message.role, content: text || (calls.length ? null : ''), ...(calls.length ? { tool_calls: calls } : {}), ...(message.role === 'tool' ? { tool_call_id: message.toolCallId } : {}) });
  }
  return result;
}

async function* sse(body) {
  const reader = body.getReader(), decoder = new TextDecoder(); let buffer = '';
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      buffer += decoder.decode(next.value, { stream: true });
      if (buffer.length > 2 * 1024 * 1024) throw badWire();
      let match;
      while ((match = /\r?\n\r?\n/.exec(buffer))) {
        const frame = buffer.slice(0, match.index); buffer = buffer.slice(match.index + match[0].length);
        const data = frame.split(/\r?\n/).filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart()).join('\n');
        if (data) yield data;
      }
    }
  } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
}

export function apply(ctx, raw) {
  const config = Config(raw);
  let ref;
  try { ref = credentialRef(config.apiKeyEnv); }
  catch { throw new LlmError('apiKeyEnv must name a credential reference, never contain a key.', 'INVALID_CONFIG'); }
  const resolveKey = async () => {
    const credentials = ctx.get('credentials');
    const value = credentials ? (await credentials.resolve(ref))?.value : launchEnvironmentOf(ctx).get(ref)?.value;
    if (!value) throw new LlmError('Configure the Local Runtime model credential reference.', 'MISSING_CREDENTIAL');
    return value;
  };
  ctx.llm.registerAdapter([config.provider], new LocalRuntimeAdapter(config, resolveKey));
}
