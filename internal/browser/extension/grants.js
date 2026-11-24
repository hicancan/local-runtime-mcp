import { packagedConfig } from './runtime_config.js';
async function postGrant(payload) {
    const settings = await chrome.storage.local.get({ address: packagedConfig.address || '127.0.0.1:9315', token: packagedConfig.token || '' });
    if (!settings.token)
        throw new Error('not configured');
    const response = await fetch(`http://${settings.address}/v1/grants`, {
        method: 'POST', headers: { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
    });
    const body = await response.json().catch(() => null);
    if (!response.ok)
        throw new Error(body?.error || `lrmcp ${response.status}`);
    return body;
}
/** Asks lrmcp for every grant and runtime status, opened on one page origin so the answer names that page's site. */
export async function grants(method, origin, grant, name) {
    if (method === 'get')
        return postGrant({ method: 'get', origin });
    if (method === 'set')
        return postGrant({ method: 'set', origin, grant: grant });
    return postGrant({ method, origin, name: name });
}
export async function toggleTunnel(origin, name, up, ttlSeconds = 0) {
    return up
        ? postGrant({ method: 'tunnel_up', origin, name, ttl_seconds: ttlSeconds })
        : postGrant({ method: 'tunnel_down', origin, name });
}
export async function setRemoteGrant(origin, target, remote) {
    return postGrant({ method: 'set_remote_grant', origin, target, remote });
}
export async function stopDesktop(origin) {
    return postGrant({ method: 'stop_desktop', origin });
}
export async function mintStudioURL(origin) {
    const settings = await chrome.storage.local.get({ address: packagedConfig.address || '127.0.0.1:9315', token: packagedConfig.token || '' });
    if (!settings.token)
        throw new Error('not configured');
    const response = await fetch(`http://${settings.address}/v1/grants`, {
        method: 'POST', headers: { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ method: 'ui_bootstrap', origin }),
    });
    const body = await response.json().catch(() => null);
    if (!response.ok || !body?.url)
        throw new Error(body?.error || `lrmcp ${response.status}`);
    return body.url;
}
/** Whether a grant's site covers a page origin: the same origin, or a subdomain under the same scheme. A site with a
 * port, or a local address, covers only itself. lrmcp names sites; this only matches them. */
export function covers(site, origin) {
    try {
        const s = new URL(site), o = new URL(origin);
        if (s.protocol !== o.protocol)
            return false;
        if (s.port || o.port)
            return s.host === o.host;
        return o.hostname === s.hostname || o.hostname.endsWith(`.${s.hostname}`);
    }
    catch {
        return false;
    }
}
/** What one page does through the gateway: uses capabilities, provides its own, or both. */
export function roles(view, origin) {
    if (!view)
        return { uses: false, provides: false };
    const own = view.grants.find(grant => covers(grant.Site, origin));
    return { uses: Boolean(own?.Use), provides: Boolean(own?.Provide) };
}
