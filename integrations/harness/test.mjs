import test from 'node:test';
import assert from 'node:assert/strict';
import { LlmAdapter, attributionHeaders } from '@deepseek-ai/dsh-llm';
import { LocalRuntimeAdapter, apply } from './index.mjs';

const config = { provider: 'local-runtime', baseURL: 'http://127.0.0.1:9317/v1', apiKeyEnv: 'LOCAL_RUNTIME_MCP_CLIENT_TOKEN', models: [{ id: 'web-test' }] };
const options = () => ({ provider: 'local-runtime', model: 'web-test', sessionId:'session-a', messages: [{ role: 'user', content: [{ type: 'text', text: 'question' }] }] });
const frame = (delta = {}, finish = null, conversation = 'session-a') => `data: ${JSON.stringify({ conversation_id: conversation, choices: [{ index: 0, delta, finish_reason: finish }] })}\r\n\r\n`;
function response(data) { return new Response(data, { headers: { 'Content-Type': 'text/event-stream' } }); }
const textResponse = (conversation = 'session-a') => response(frame({ content: 'answer' }, null, conversation) + frame({}, 'stop', conversation) + 'data: [DONE]\r\n\r\n');
async function collect(adapter, input = options()) { return Array.fromAsync(adapter.stream(input)); }

test('official provider interface advertises no invented token/context defaults and disables replay', async () => {
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async () => textResponse());
  assert.equal(adapter instanceof LlmAdapter, true);
  const model = await adapter.resolveModel('local-runtime', 'web-test');
  assert.equal('defaultMaxTokens' in model, false);
  assert.equal('context' in model, false);
  assert.equal('reasoning' in model, false);
  assert.deepEqual(model.inputModalities, ['text']);
  assert.equal(adapter.providerRetryPolicy().maxRetries, 0);
});

test('actual outgoing wire preserves text, system, tool schemas and correlated history', async () => {
  let sent;
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async (url, init) => { sent = { url, init, body: JSON.parse(init.body) }; return textResponse(); });
  const input = options();
  input.system = 'system text';
  input.messages.push({ role: 'assistant', content: [{ type: 'text', text: 'before tool' }, { type: 'tool-call', id: 'call-1', name: 'read_file', arguments: '{"path":"proof.txt"}' }] });
  input.messages.push({ role: 'tool', toolCallId: 'call-1', content: [{ type: 'text', text: 'file answer' }] });
  input.tools = [{ name: 'read_file', description: 'Read', parameters: { type: 'object' } }];
  const chunks = await collect(adapter, input);
  assert.equal(sent.url, config.baseURL + '/chat/completions');
  for (const [key, value] of Object.entries(attributionHeaders())) assert.equal(sent.init.headers[key], value);
  assert.equal(sent.init.headers.Authorization.startsWith('Bearer '), true);
  for (const field of ['max_tokens', 'max_completion_tokens', 'temperature', 'stream_options', 'reasoning_effort']) assert.equal(field in sent.body, false);
  assert.equal(sent.body.conversation_id, 'session-a');
  assert.deepEqual(sent.body.messages[0], { role: 'system', content: 'system text' });
  assert.equal(sent.body.messages[2].tool_calls[0].id, 'call-1');
  assert.equal(sent.body.messages[3].tool_call_id, 'call-1');
  assert.deepEqual(sent.body.tools[0].function.parameters, { type: 'object' });
  assert.equal(chunks.at(-1).reason.kind, 'stop');
});

test('real Harness session identity persists across tool rounds and is never invented', async () => {
  const bodies = [];
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async (_, init) => { const body = JSON.parse(init.body); bodies.push(body); return textResponse(body.conversation_id); });
  await collect(adapter, { ...options(), sessionId: 'session-a' });
  await collect(adapter, { ...options(), sessionId: 'session-a', messages: [...options().messages, { role: 'assistant', content: [{ type: 'tool-call', id: 'call-a', name: 'read_file', arguments: '{"path":"proof.txt"}' }] }, { role: 'tool', toolCallId: 'call-a', content: [{ type: 'text', text: 'tool result' }] }] });
  await collect(adapter, { ...options(), sessionId: 'session-b' });
  await assert.rejects(collect(adapter,{...options(),sessionId:undefined}),error=>error.code==='INVALID_ARGS');
  assert.deepEqual(bodies.map(body => body.conversation_id), ['session-a', 'session-a', 'session-b']);
  assert.equal(bodies[1].messages.at(-1).tool_call_id, 'call-a');
});

test('a different or absent conversation ID cannot be accepted as this session answer', async () => {
  for (const wire of [frame({ content: 'wrong session' }, null, 'session-b'), 'data: {"choices":[{"index":0,"delta":{"content":"missing session"},"finish_reason":null}]}\n\n']) {
    const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async () => response(wire));
    const seen = [];
    await assert.rejects(async () => { for await (const chunk of adapter.stream(options())) seen.push(chunk); }, error => error.code === 'MALFORMED_RESPONSE');
    assert.equal(seen.some(chunk => chunk.type === 'text-delta' || chunk.type === 'finish'), false);
  }
});

test('explicit unsupported controls fail before credentials or fetch', async () => {
  for (const [field, value] of [['maxTokens', 50], ['temperature', 0], ['stop', []], ['reasoningEffort', 'off']]) {
    let touched = false;
    const adapter = new LocalRuntimeAdapter(config, async () => { touched = true; }, async () => { touched = true; });
    await assert.rejects(collect(adapter, { ...options(), [field]: value }), error => error.code === 'UNSUPPORTED_OPTION');
    assert.equal(touched, false);
  }
});

test('unsupported image content is refused rather than silently dropped', async () => {
  let fetched = false;
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async () => { fetched = true; });
  await assert.rejects(collect(adapter, { ...options(), messages: [{ role: 'user', content: [{ type: 'image', attachment: {} }] }] }), error => error.code === 'UNSUPPORTED_CONTENT');
  assert.equal(fetched, false);
});

test('SSE UTF-8 bytes and comments preserve real text deltas and completion', async () => {
  const data = new TextEncoder().encode(': heartbeat\r\n\r\n' + frame({ content: '你好' }) + frame({}, 'stop') + 'data: [DONE]\r\n\r\n');
  const body = new ReadableStream({ start(controller) { for (const byte of data) controller.enqueue(new Uint8Array([byte])); controller.close(); } });
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async () => response(body));
  const chunks = await collect(adapter);
  assert.equal(chunks.filter(chunk => chunk.type === 'text-delta').map(chunk => chunk.text).join(''), '你好');
  assert.deepEqual(chunks.find(chunk => chunk.type === 'block-end').block, { type: 'text', text: '你好' });
  assert.equal(chunks.at(-1).reason.kind, 'stop');
});

test('fragmented standard tool calls become Harness blocks without executing them', async () => {
  const wire = frame({ tool_calls: [{ index: 0, id: 'call-a', type: 'function', function: { name: 'read_file', arguments: '{"path":' } }] })
    + frame({ tool_calls: [{ index: 0, function: { arguments: '"proof.txt"}' } }] }) + frame({}, 'tool_calls') + 'data: [DONE]\n\n';
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async () => response(wire));
  const chunks = await collect(adapter, { ...options(), tools: [{ name: 'read_file', parameters: { type: 'object' } }] });
  const call = chunks.find(chunk => chunk.type === 'block-end').block;
  assert.deepEqual(call, { type: 'tool-call', id: 'call-a', name: 'read_file', arguments: '{"path":"proof.txt"}' });
  assert.equal(chunks.at(-1).reason.kind, 'tool-calls');
});

test('truncated/error streams never report success or disclose remote detail', async () => {
  for (const wire of [frame({ content: 'partial' }), frame({}, 'stop'), 'data: {"error":{"message":"sensitive-fixture-detail"}}\n\n']) {
    const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async () => response(wire));
    const seen = [];
    await assert.rejects(async () => { for await (const chunk of adapter.stream(options())) seen.push(chunk); }, error => !error.message.includes('sensitive-fixture-detail'));
    assert.equal(seen.some(chunk => chunk.type === 'finish'), false);
  }
});

test('stopping the stream aborts its real fetch signal and cancels the reader', async () => {
  let signal, cancelled = false;
  const body = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode(frame({ content: 'first' }))); }, cancel() { cancelled = true; } });
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async (_, init) => { signal = init.signal; return response(body); });
  const stream = adapter.stream(options());
  assert.equal((await stream.next()).value.type, 'block-start');
  await stream.return();
  assert.equal(signal.aborted, true);
  assert.equal(cancelled, true);
});

test('known provider states produce helpful fixed messages rather than raw page errors', async () => {
  const adapter = new LocalRuntimeAdapter(config, async () => 'fixture-model-key', async () => response('data: {"conversation_id":"session-a","error":{"code":"provider_unavailable","message":"sensitive-fixture-detail"}}\n\n'));
  await assert.rejects(collect(adapter), error => error.code === 'SERVER' && error.message.includes('website is temporarily unavailable') && !error.message.includes('sensitive-fixture-detail'));
});

test('plugin mounts through the official registry and resolves only its credential reference', async () => {
  let registration;
  apply({ llm: { registerAdapter(routes, adapter) { registration = { routes, adapter }; } }, get() { return { resolve: async ref => { assert.equal(ref, 'LOCAL_RUNTIME_MCP_CLIENT_TOKEN'); return { value: 'fixture-model-key' }; } }; } }, { models: [{ id: 'web-test' }] });
  assert.deepEqual(registration.routes, ['local-runtime']);
  assert.equal(registration.adapter instanceof LlmAdapter, true);
  assert.equal(await registration.adapter.resolveKey(), 'fixture-model-key');
});
