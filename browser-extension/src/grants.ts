import { packagedConfig } from './runtime_config.js';

// The owner's grant panel and the toolbar icon read and change web grants through lrmcp's bridge, with the credential
// only this extension holds. No web page reaches this path.
export type Definition = { name: string; title?: string; description: string; annotations: { readOnlyHint: boolean; destructiveHint: boolean }; origins?: string[] };
export type Grant = { Origin: string; Models?: string[]; Capabilities?: Record<string, string>; Tools?: string[] };
export type View = { grants: Grant[]; pageTools: Definition[]; machine: Definition[]; models: { id: string; title?: string }[] };

export async function grants(method: 'get' | 'set', grant?: Grant): Promise<View> {
  const settings = await chrome.storage.local.get({ address: packagedConfig.address || '127.0.0.1:9315', token: packagedConfig.token || '' });
  if (!settings.token) throw new Error('not configured');
  const response = await fetch(`http://${settings.address}/v1/grants`, {
    method: 'POST', headers: { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ method, ...(grant ? { grant } : {}) }),
  });
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error(body?.error || `lrmcp ${response.status}`);
  return body as View;
}

/** What one site does through the gateway: uses its capabilities, provides its own, or both. */
export function roles(view: View | null, origin: string): { uses: boolean; provides: boolean } {
  if (!view) return { uses: false, provides: false };
  const own = view.grants.find(grant => grant.Origin === origin);
  const uses = Boolean(own && ((own.Models?.length ?? 0) + Object.keys(own.Capabilities ?? {}).length + (own.Tools?.length ?? 0) > 0));
  const provides = view.grants.some(grant => Object.values(grant.Capabilities ?? {}).some(target => { try { return new URL(target).origin === origin; } catch { return false; } }));
  return { uses, provides };
}
