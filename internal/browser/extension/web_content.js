"use strict";
// Fixed top-level bridge: forwards bounded messages only; never reads page content.
// This is a classic content script, deliberately without module imports.
(() => {
    const protocol = 'lrmcp.web.v1';
    if (window !== window.top || !['http:', 'https:'].includes(location.protocol))
        return;
    let port;
    const pending = new Set();
    const reply = (value) => window.postMessage(value, location.origin);
    const connect = () => {
        if (port)
            return port;
        port = chrome.runtime.connect({ name: protocol });
        port.onMessage.addListener(message => {
            if (message?.protocol !== protocol || !pending.has(message.id) || !['event', 'result', 'error'].includes(message.kind))
                return;
            if (message.kind !== 'event')
                pending.delete(message.id);
            reply(message);
        });
        port.onDisconnect.addListener(() => {
            void chrome.runtime.lastError;
            port = undefined;
            for (const id of pending)
                reply({ protocol, id, kind: 'error', error: 'local runtime disconnected; outcome may be unknown' });
            pending.clear();
        });
        return port;
    };
    window.addEventListener('message', event => {
        if (event.source !== window || event.origin !== location.origin)
            return;
        const message = event.data;
        if (!message || message.protocol !== protocol || typeof message.id !== 'string' || !/^[A-Za-z0-9_-]{1,100}$/.test(message.id))
            return;
        if (message.kind === 'cancel') {
            if (pending.has(message.id))
                port?.postMessage({ protocol, kind: 'cancel', id: message.id });
            return;
        }
        if (message.kind !== 'request' || !['capabilities.list', 'capabilities.invoke', 'models.list', 'models.chat', 'models.close'].includes(message.method)
            || !message.params || typeof message.params !== 'object' || Array.isArray(message.params) || pending.has(message.id))
            return;
        try {
            if (JSON.stringify(message.params).length > 1024 * 1024 || pending.size >= 8)
                throw new Error('web request capacity exceeded');
            pending.add(message.id);
            connect().postMessage({ protocol, kind: 'request', id: message.id, method: message.method, params: message.params });
        }
        catch {
            pending.delete(message.id);
            reply({ protocol, kind: 'error', id: message.id, error: 'local web bridge unavailable or request capacity exceeded' });
        }
    });
    window.addEventListener('pagehide', () => { port?.disconnect(); port = undefined; pending.clear(); }, { once: true });
})();
