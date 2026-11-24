import { packagedConfig } from './runtime_config.js';

// The owner's panel and toolbar icon read and change site grants, remote wire
// switches, remote read-only policies, and pending proposals through lrmcp's
// authenticated loopback bridge. No web page reaches this path.
export type Definition = { name: string; title?: string; description: string; annotations: { readOnlyHint: boolean; destructiveHint: boolean }; origins?: string[] };
export type Grant = { Site: string; Use?: boolean; Provide?: boolean; Capabilities?: Record<string, string>; Tools?: string[] };
/** A page tool an AI wrote down after exploring a site, waiting for the owner. */
export type Proposal = { definition: Definition; script: string; replaces: boolean };
export type TunnelStatus = {
  name: 'openai' | 'cloudflare' | 'http' | string;
  configured: boolean;
  state: 'stopped' | 'active' | 'error' | string;
  address?: string;
  started_at?: string;
  expires_at?: string;
  ttl_remaining_seconds?: number;
  last_error?: string;
};
export type RemoteGrant = { read_only?: boolean; tools?: string[] };
export type RemoteGrants = { openai: RemoteGrant; http: RemoteGrant };
export type RuntimeSummary = { attached_clients: number; desktop_owned: boolean; ui_available?: boolean };
export type View = {
  site: string;
  grants: Grant[];
  pageTools: Definition[];
  machine: Definition[];
  proposals: Proposal[];
  tunnels?: TunnelStatus[];
  remoteGrants?: RemoteGrants;
  runtime?: RuntimeSummary;
};

type GrantAction =
  | { method: 'get'; origin: string }
  | { method: 'set'; origin: string; grant: Grant }
  | { method: 'approve' | 'reject'; origin: string; name: string }
  | { method: 'tunnel_up'; origin: string; name: string; ttl_seconds?: number }
  | { method: 'tunnel_down'; origin: string; name: string }
  | { method: 'set_remote_grant'; origin: string; target: 'openai' | 'http'; remote: RemoteGrant }
  | { method: 'stop_desktop'; origin: string }
  | { method: 'ui_bootstrap'; origin: string };

async function postGrant(payload: GrantAction): Promise<View> {
  const settings = await chrome.storage.local.get({ address: packagedConfig.address || '127.0.0.1:9315', token: packagedConfig.token || '' });
  if (!settings.token) throw new Error('not configured');
  const response = await fetch(`http://${settings.address}/v1/grants`, {
    method: 'POST', headers: { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error(body?.error || `lrmcp ${response.status}`);
  return body as View;
}

/** Asks lrmcp for every grant and runtime status, opened on one page origin so the answer names that page's site. */
export async function grants(method: 'get' | 'set' | 'approve' | 'reject', origin: string, grant?: Grant, name?: string): Promise<View> {
  if (method === 'get') return postGrant({ method: 'get', origin });
  if (method === 'set') return postGrant({ method: 'set', origin, grant: grant! });
  return postGrant({ method, origin, name: name! });
}

export async function toggleTunnel(origin: string, name: string, up: boolean, ttlSeconds = 0): Promise<View> {
  return up
    ? postGrant({ method: 'tunnel_up', origin, name, ttl_seconds: ttlSeconds })
    : postGrant({ method: 'tunnel_down', origin, name });
}

export async function setRemoteGrant(origin: string, target: 'openai' | 'http', remote: RemoteGrant): Promise<View> {
  return postGrant({ method: 'set_remote_grant', origin, target, remote });
}

export async function stopDesktop(origin: string): Promise<View> {
  return postGrant({ method: 'stop_desktop', origin });
}

export async function mintStudioURL(origin: string): Promise<string> {
  const settings = await chrome.storage.local.get({ address: packagedConfig.address || '127.0.0.1:9315', token: packagedConfig.token || '' });
  if (!settings.token) throw new Error('not configured');
  const response = await fetch(`http://${settings.address}/v1/grants`, {
    method: 'POST', headers: { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ method: 'ui_bootstrap', origin }),
  });
  const body = await response.json().catch(() => null);
  if (!response.ok || !body?.url) throw new Error(body?.error || `lrmcp ${response.status}`);
  return body.url as string;
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
