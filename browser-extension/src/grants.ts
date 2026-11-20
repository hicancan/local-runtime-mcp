import { packagedConfig } from './runtime_config.js';

// The owner's grant panel and the toolbar icon read and change web grants through lrmcp's bridge, with the credential
// only this extension holds. No web page reaches this path.
export type Definition = { name: string; title?: string; description: string; annotations: { readOnlyHint: boolean; destructiveHint: boolean }; origins?: string[] };
export type Grant = { Site: string; Use?: boolean; Provide?: boolean; Models?: string[]; Capabilities?: Record<string, string>; Tools?: string[] };
export type View = { site: string; grants: Grant[]; pageTools: Definition[]; machine: Definition[]; models: { id: string; title?: string }[] };

/** Asks lrmcp for every grant, opened on one page origin so the answer names that page's site. */
export async function grants(method: 'get' | 'set', origin: string, grant?: Grant): Promise<View> {
  const settings = await chrome.storage.local.get({ address: packagedConfig.address || '127.0.0.1:9315', token: packagedConfig.token || '' });
  if (!settings.token) throw new Error('not configured');
  const response = await fetch(`http://${settings.address}/v1/grants`, {
    method: 'POST', headers: { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ method, origin, ...(grant ? { grant } : {}) }),
  });
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error(body?.error || `lrmcp ${response.status}`);
  return body as View;
}

/** Whether a grant's site covers a page origin: the same origin, or a subdomain under the same scheme. A site with a
 * port, or a local address, covers only itself. lrmcp names sites; this only matches them. */
export function covers(site: string, origin: string): boolean {
  try {
    const s = new URL(site), o = new URL(origin);
    if (s.protocol !== o.protocol) return false;
    if (s.port || o.port) return s.host === o.host;
    return o.hostname === s.hostname || o.hostname.endsWith(`.${s.hostname}`);
  } catch { return false; }
}

/** What one page does through the gateway: uses capabilities, provides its own, or both. */
export function roles(view: View | null, origin: string): { uses: boolean; provides: boolean } {
  if (!view) return { uses: false, provides: false };
  const own = view.grants.find(grant => covers(grant.Site, origin));
  return { uses: Boolean(own?.Use), provides: Boolean(own?.Provide) };
}
