import assert from 'node:assert/strict';
import { test } from 'node:test';
import { LaneScheduler } from '../../internal/browser/extension/scheduler.js';

const deferred = () => { let resolve; return { promise: new Promise(r => resolve = r), release: () => resolve() }; };
const tick = () => new Promise(r => setImmediate(r));
const deadline = () => Date.now() + 10000;

test('busy lanes do not consume slots or starve independent tabs', async () => {
  const scheduler = new LaneScheduler(2, 8), held = deferred(), starts = [];
  const a = scheduler.submit('a1', 'a', deadline(), async () => { starts.push('a1'); await held.promise; });
  const a2 = scheduler.submit('a2', 'a', deadline(), async () => { starts.push('a2'); });
  const a3 = scheduler.submit('a3', 'a', deadline(), async () => { starts.push('a3'); });
  const b = scheduler.submit('b', 'b', deadline(), async () => { starts.push('b'); });
  await b; assert.deepEqual(starts, ['a1', 'b']);
  held.release(); await Promise.all([a, a2, a3]); assert.deepEqual(starts, ['a1', 'b', 'a2', 'a3']);
});

test('queued cancellation never dispatches, active cancellation retains lane until cleanup', async () => {
  const scheduler = new LaneScheduler(1, 4), held = deferred(); let dispatched = false;
  const first = scheduler.submit('first', 'tab', deadline(), async signal => { await held.promise; signal.throwIfAborted(); });
  const firstRejected = assert.rejects(first, /canceled/);
  const queued = scheduler.submit('queued', 'tab', deadline(), async () => { dispatched = true; });
  const rejected = assert.rejects(queued, /canceled/); scheduler.cancel('queued'); await rejected;
  scheduler.cancel('first'); await tick(); assert.equal(dispatched, false);
  held.release(); await firstRejected;
});

test('deadlines, duplicate IDs and bounded capacity are enforced', async () => {
  const scheduler = new LaneScheduler(1, 1), held = deferred();
  const first = scheduler.submit('one', 'a', deadline(), async () => held.promise);
  await assert.rejects(scheduler.submit('one', 'a', deadline(), async () => {}), /duplicate/);
  await assert.rejects(scheduler.submit('two', 'b', deadline(), async () => {}), /capacity/);
  held.release(); await first;
  await assert.rejects(scheduler.submit('old', 'b', Date.now()-1, async () => {}), /deadline/);
});
