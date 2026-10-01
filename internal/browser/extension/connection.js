// WebSocket messages reset Chromium MV3's idle timer (Chromium 116+). The
// bridge sends one keepalive every 20 seconds; no tab/debugger is retained.
export function commandStream(address, token, peer, signal, ready, command) {
    return new Promise((resolve, reject) => {
        const socket = new WebSocket(`ws://${address}/v1/connect`);
        let authenticated = false, settled = false;
        let watchdog = setTimeout(() => finish(new Error('browser bridge authentication timed out')), 5000);
        const finish = (error) => {
            if (settled)
                return;
            settled = true;
            clearTimeout(watchdog);
            signal.removeEventListener('abort', abort);
            socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null;
            socket.close();
            error ? reject(error) : resolve();
        };
        const abort = () => finish();
        signal.addEventListener('abort', abort, { once: true });
        if (signal.aborted) {
            finish();
            return;
        }
        socket.onopen = () => socket.send(JSON.stringify({ type: 'authenticate', token, peer }));
        socket.onerror = () => finish(new Error('browser bridge connection failed'));
        socket.onclose = () => finish(new Error('browser bridge connection closed'));
        socket.onmessage = event => {
            if (settled)
                return;
            try {
                if (typeof event.data !== 'string' || event.data.length > 32 * 1024 * 1024)
                    throw new Error('invalid bridge message');
                const message = JSON.parse(event.data);
                if (message.type === 'error')
                    throw new Error(message.error || 'browser bridge rejected connection');
                if (!authenticated) {
                    if (message.type !== 'ready' || typeof message.bridge_id !== 'string' || !message.bridge_id)
                        throw new Error('invalid bridge authentication response');
                    authenticated = true;
                    ready(message.bridge_id);
                }
                else if (message.type === 'keepalive') {
                    socket.send(JSON.stringify({ type: 'keepalive' }));
                }
                else {
                    if (message.boot_id !== peer.boot_id || typeof message.id !== 'string' || typeof message.method !== 'string')
                        throw new Error('command generation is stale');
                    command(message);
                }
                clearTimeout(watchdog);
                watchdog = setTimeout(() => finish(new Error('browser bridge keepalive timed out')), 35000);
            }
            catch (error) {
                finish(error instanceof Error ? error : new Error('invalid bridge message'));
            }
        };
    });
}
