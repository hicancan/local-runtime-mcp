export const WEB_PROTOCOL = 'lrmcp.web.v1';
export const WEB_METHODS = new Set(['capabilities.list', 'capabilities.invoke', 'models.list', 'models.chat', 'models.close']);
export function webMessage(value) {
    if (!value || typeof value !== 'object')
        return null;
    const v = value;
    if (v.protocol !== WEB_PROTOCOL || typeof v.id !== 'string' || !/^[A-Za-z0-9_-]{1,100}$/.test(v.id))
        return null;
    if (v.kind === 'cancel')
        return { protocol: WEB_PROTOCOL, kind: 'cancel', id: v.id };
    if (v.kind !== 'request' || typeof v.method !== 'string' || !WEB_METHODS.has(v.method)
        || !v.params || typeof v.params !== 'object' || Array.isArray(v.params))
        return null;
    try {
        if (JSON.stringify(v.params).length > 1024 * 1024)
            return null;
    }
    catch {
        return null;
    }
    // Do not forward caller-supplied origin, tab, frame, credentials or script fields.
    return { protocol: WEB_PROTOCOL, kind: 'request', id: v.id, method: v.method, params: v.params };
}
export function webSource(sender, extensionID) {
    if (sender.id !== extensionID || sender.frameId !== 0 || !sender.tab?.id || !sender.url || !sender.origin || !sender.documentId) {
        throw new Error('web bridge requires a top-level content-script sender');
    }
    const url = new URL(sender.url);
    if (!['https:', 'http:'].includes(url.protocol) || url.origin !== sender.origin || sender.origin === 'null'
        || url.username || url.password)
        throw new Error('web bridge sender origin is invalid');
    return { origin: url.origin, url: url.href, frame_id: 0, document_id: sender.documentId, nativeTabID: sender.tab.id };
}
/** Consume bounded NDJSON without buffering a whole model response. */
export async function readWebStream(response, emit) {
    if (!response.ok)
        throw new Error(`local web bridge returned HTTP ${response.status}`);
    if (!response.headers.get('content-type')?.startsWith('application/x-ndjson') || !response.body)
        throw new Error('invalid local web bridge stream');
    const reader = response.body.getReader(), decoder = new TextDecoder();
    let buffer = '', ended = false, total = 0;
    const line = (text) => {
        if (!text.trim())
            return;
        if (ended)
            throw new Error('web bridge sent data after its final result');
        const item = JSON.parse(text);
        if (!item || !['event', 'result', 'error'].includes(String(item.type)))
            throw new Error('invalid web bridge event');
        if (item.type === 'error' && typeof item.error !== 'string')
            throw new Error('invalid web bridge error');
        if (item.code !== undefined && (typeof item.code !== 'string' || !/^[a-z][a-z0-9_]{0,79}$/.test(item.code)))
            throw new Error('invalid web bridge error code');
        if (item.type !== 'event')
            ended = true;
        emit(item);
    };
    try {
        while (true) {
            const next = await reader.read();
            if (next.done)
                break;
            total += next.value.byteLength;
            if (total > 64 * 1024 * 1024)
                throw new Error('web bridge response exceeded 64 MiB');
            buffer += decoder.decode(next.value, { stream: true });
            if (buffer.length > 2 * 1024 * 1024)
                throw new Error('web bridge event exceeded 2 MiB');
            let newline;
            while ((newline = buffer.indexOf('\n')) >= 0) {
                line(buffer.slice(0, newline));
                buffer = buffer.slice(newline + 1);
            }
        }
        buffer += decoder.decode();
        if (buffer.trim())
            line(buffer);
        if (!ended)
            throw new Error('web bridge disconnected before confirming a result; outcome may be unknown');
    }
    finally {
        await reader.cancel().catch(() => { });
        reader.releaseLock();
    }
}
