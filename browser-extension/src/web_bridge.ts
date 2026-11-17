import { WEB_PROTOCOL, webMessage, webSource, readWebStream } from './web_protocol.js';

type Identity = { address: string; token: string; instanceId: string; bootId: string };

/** No page receives the loopback credential. Every request is bound to Port.sender. */
export function installWebBridge(identity: () => Promise<Identity>, tabHandle: (id: number) => string) {
  chrome.runtime.onConnect.addListener(port => {
    if (port.name !== WEB_PROTOCOL) return;
    let source: ReturnType<typeof webSource>;
    try { source = webSource(port.sender || {}, chrome.runtime.id); } catch { port.disconnect(); return; }
    const calls = new Map<string, AbortController>();
    const seen = new Set<string>();
    let closed = false;
    const post = (value: unknown) => { if (!closed) { try { port.postMessage(value); } catch { /* disconnected document */ } } };
    port.onDisconnect.addListener(() => { closed = true; for (const call of calls.values()) call.abort(); calls.clear(); });
    port.onMessage.addListener(raw => {
      const message = webMessage(raw);
      if (!message) return;
      if (message.kind === 'cancel') { calls.get(message.id)?.abort(new Error('page canceled its web request; remote outcome may be unknown')); return; }
      if (seen.has(message.id) || calls.size >= 8 || seen.size >= 512) {
        post({ protocol: WEB_PROTOCOL, id: message.id, kind: 'error', error: 'duplicate request or web bridge document capacity reached' }); return;
      }
      seen.add(message.id);
      const controller = new AbortController(); calls.set(message.id, controller);
      const timeout = setTimeout(() => controller.abort(new Error('web request deadline expired; outcome may be unknown')), 10 * 60_000);
      void (async () => {
        const settings = await identity(); controller.signal.throwIfAborted();
        if (!settings.token || settings.token.length < 32 || !settings.instanceId) throw new Error('local runtime browser bridge is not configured');
        const address = new URL(`http://${settings.address}`);
        if (!['localhost', '127.0.0.1', '[::1]'].includes(address.hostname) || address.username || address.password
          || address.pathname !== '/' || address.search || address.hash) throw new Error('local runtime address must be loopback');
        const { nativeTabID, ...sender } = source;
        const response = await fetch(`${address.origin}/v1/web`, {
          method: 'POST', headers: { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' },
          body: JSON.stringify({ instance_id: settings.instanceId, boot_id: settings.bootId,
            source: { ...sender, tab_id: tabHandle(nativeTabID) }, request: { id: message.id, method: message.method, params: message.params } }),
          signal: controller.signal,
        });
        await readWebStream(response, item => post({ protocol: WEB_PROTOCOL, id: message.id,
          kind: item.type, ...(item.type === 'error' ? { error: item.error, ...(item.code ? { code: item.code } : {}) } : { data: item.type === 'result' ? item.result : item.data }) }));
      })().catch(error => post({ protocol: WEB_PROTOCOL, id: message.id, kind: 'error',
        error: controller.signal.aborted ? 'web request canceled or timed out; remote outcome may be unknown' : error instanceof Error ? error.message : 'web bridge failed' }))
        .finally(() => { clearTimeout(timeout); calls.delete(message.id); });
    });
  });
}
