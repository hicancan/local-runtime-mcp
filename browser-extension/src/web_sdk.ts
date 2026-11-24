import { WEB_PROTOCOL } from './web_protocol.js';

export type LocalRuntimeEvent = { type: string; [key: string]: unknown };
export type WebCallOptions = { signal?: AbortSignal; timeoutMs?: number };
export type WebCall = AsyncIterable<LocalRuntimeEvent> & { result: Promise<unknown>; cancel(): void };

export class LocalRuntimeError extends Error {
  constructor(message: string, public readonly code?: string) { super(message); this.name = 'LocalRuntimeError'; }
}

/** Site-owned SDK; no extension ID, local credential, eval, or target-tab parameter. */
export function createLRMCPClient(target: Window = window) {
  function request(method: string, params: Record<string, unknown>, options: WebCallOptions = {}): WebCall {
    const id = crypto.randomUUID();
    const queue: LocalRuntimeEvent[] = [];
    let wake: (() => void) | undefined, ended = false, failure: Error | undefined;
    let resolve!: (value: unknown) => void, reject!: (error: unknown) => void;
    const result = new Promise<unknown>((yes, no) => { resolve = yes; reject = no; });
    // Iteration-only consumers must not cause an unhandled rejection.
    void result.catch(() => {});
    const cleanup = () => { clearTimeout(timer); target.removeEventListener('message', receive); options.signal?.removeEventListener('abort', cancel); wake?.(); };
    const stop = (error: Error) => { if (ended) return; ended = true; failure = error; reject(error); cleanup(); };
    const cancel = () => { if (ended) return; target.postMessage({ protocol: WEB_PROTOCOL, kind: 'cancel', id }, target.location.origin); stop(new Error('local runtime request canceled; remote outcome may be unknown')); };
    const receive = (event: MessageEvent) => {
      const value = event.data;
      if (event.source !== target || event.origin !== target.location.origin || value?.protocol !== WEB_PROTOCOL || value.id !== id || ended) return;
      if (value.kind === 'event') {
        if (!value.data || typeof value.data !== 'object' || typeof value.data.type !== 'string') { stop(new Error('invalid local runtime event')); return; }
        if (queue.length >= 2048) { cancel(); return; }
        queue.push(value.data); wake?.();
      } else if (value.kind === 'result') { ended = true; resolve(value.data); cleanup(); }
      else if (value.kind === 'error') stop(new LocalRuntimeError(typeof value.error === 'string' ? value.error : 'local runtime request failed', typeof value.code === 'string' ? value.code : undefined));
    };
    const timeoutMs = Math.min(10 * 60_000, Math.max(1000, options.timeoutMs ?? 5 * 60_000));
    const timer = setTimeout(cancel, timeoutMs);
    target.addEventListener('message', receive);
    options.signal?.addEventListener('abort', cancel, { once: true });
    if (options.signal?.aborted) cancel();
    else target.postMessage({ protocol: WEB_PROTOCOL, kind: 'request', id, method, params }, target.location.origin);
    return { result, cancel, async *[Symbol.asyncIterator]() {
      try {
        while (true) {
          if (queue.length) { yield queue.shift()!; continue; }
          if (ended) { if (failure) throw failure; return; }
          await new Promise<void>(done => { wake = done; }); wake = undefined;
        }
      } finally { if (!ended) cancel(); }
    } };
  }
  return {
    request,
    listCapabilities: (options?: WebCallOptions) => request('capabilities.list', {}, options).result,
    invoke: (name: string, input: Record<string, unknown>, options?: WebCallOptions) => request('capabilities.invoke', { name, input }, options).result,
  };
}
