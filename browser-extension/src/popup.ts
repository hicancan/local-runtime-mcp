import { covers, grants, type Definition, type Grant, type View } from './grants.js';

// The owner's panel for the site in the current tab: what it may use of this computer (blue) and which of its own page
// tools it offers to other sites (green). Every change goes to lrmcp at once and lands in its configuration file.
type Messages = Record<string, { message: string }>;
let messages: Messages = {};
let language = 'en';
/** The owner's language: their choice in this panel, else Chinese whenever the system prefers it, else English. */
async function loadLanguage() {
  const saved = (await chrome.storage.local.get({ language: '' })).language as string;
  language = saved || (navigator.languages.some(l => l.toLowerCase().startsWith('zh')) ? 'zh_CN' : 'en');
  messages = await (await fetch(chrome.runtime.getURL(`_locales/${language}/messages.json`))).json();
  document.documentElement.lang = language === 'zh_CN' ? 'zh-CN' : 'en';
  for (const node of document.querySelectorAll<HTMLElement>('[data-i18n]')) node.textContent = t(node.dataset.i18n!);
  $('language').textContent = language === 'zh_CN' ? 'EN' : '中文';
}
const t = (key: string, ...subs: string[]) => (messages[key]?.message ?? '').replace(/\$[A-Z]+\$/g, () => subs.shift() ?? '') || key;
const $ = (id: string) => document.getElementById(id)!;
const status = $('status'), panel = $('panel');

$('language').addEventListener('click', async () => {
  await chrome.storage.local.set({ language: language === 'zh_CN' ? 'en' : 'zh_CN' });
  await loadLanguage(); paintConnection(); render();
});
$('settings').addEventListener('click', event => { event.preventDefault(); void chrome.runtime.openOptionsPage(); });

function el<K extends keyof HTMLElementTagNameMap>(tag: K, props: Record<string, unknown> = {}, ...children: (Node | string)[]): HTMLElementTagNameMap[K] {
  const node: HTMLElementTagNameMap[K] = document.createElement(tag);
  Object.assign(node, props);
  node.append(...children);
  return node;
}

/** A tool's name in the owner's language: the locale's translation of a built-in tool, else its own title. */
function label(d: Definition) {
  return messages[`tool_${d.name.replace(/\W/g, '_')}`]?.message || d.title || d.name;
}

function row(text: string, title: string, checked: boolean, change: (on: boolean) => void) {
  const box = el('input', { type: 'checkbox', checked });
  box.addEventListener('change', () => change(box.checked));
  return el('label', { className: 'row', title }, box, el('span', {}, text));
}

function group(title: string, rows: HTMLElement[]) {
  return rows.length ? el('div', { className: 'group' }, el('h4', {}, title), ...rows) : null;
}

/** A section heads with its dot (lit when the direction is in use) and, where one switch means something, a switch. */
function section(kind: 'uses' | 'provides', title: string, on: boolean, toggle: ((on: boolean) => void) | null, hint: string, body: (HTMLElement | null)[]) {
  const head = el('label', {}, el('span', { className: on ? 'dot on' : 'dot' }), el('span', {}, title));
  if (toggle) {
    const master = el('input', { type: 'checkbox', checked: on });
    master.addEventListener('change', () => toggle(master.checked));
    head.append(master);
  }
  const node = el('section', { className: kind }, head);
  if (on || !toggle) node.append(el('p', { className: 'hint' }, hint), ...body.filter((n): n is HTMLElement => Boolean(n)));
  return node;
}

let view: View | null = null;
let origin = '', page = '';

async function save(grant: Grant) {
  try {
    view = await grants('set', origin, grant);
    status.textContent = t('saved');
    void chrome.runtime.sendMessage({ type: 'grants-changed' }).catch(() => {});
  } catch (error) {
    status.textContent = t('failed', error instanceof Error ? error.message : String(error));
  }
  render();
}

function own(): Grant {
  const g = view!.grants.find(g => g.Site === view!.site);
  return { Site: view!.site, Use: Boolean(g?.Use), Provide: Boolean(g?.Provide), Models: [...(g?.Models ?? [])], Capabilities: { ...(g?.Capabilities ?? {}) }, Tools: [...(g?.Tools ?? [])] };
}

function flip(list: string[], item: string, on: boolean) {
  const rest = list.filter(x => x !== item);
  return on ? [...rest, item] : rest;
}

function usesSection() {
  const g = own();
  const on = Boolean(g.Use);
  const tool = (d: Definition) => row(label(d), d.description, g.Tools!.includes(d.name), checked => save({ ...g, Tools: flip(g.Tools!, d.name, checked) }));
  const reads = view!.machine.filter(d => d.annotations.readOnlyHint);
  const changes = view!.machine.filter(d => !d.annotations.readOnlyHint);
  // Page tools of the sites that provide theirs, run on the page the owner chose when offering it.
  const providers = view!.grants.filter(p => p.Provide && p.Site !== view!.site);
  const offered = view!.pageTools.filter(d => d.origins?.some(o => providers.some(p => covers(p.Site, o))));
  const pages = offered.map(d => row(label(d), d.description, d.name in g.Capabilities!, checked => {
    const capabilities = { ...g.Capabilities! };
    if (checked) capabilities[d.name] = d.origins?.[0] ? `${d.origins[0]}/` : ''; else delete capabilities[d.name];
    void save({ ...g, Capabilities: capabilities });
  }));
  const models = view!.models.map(m => row(m.title || m.id, m.id, g.Models!.includes(m.id), checked => save({ ...g, Models: flip(g.Models!, m.id, checked) })));
  return section('uses', t('usesTitle'), on, checked => {
    // The switch is the authorization. Turning it on also ticks what only reads; anything that changes the computer
    // stays one tick away. Turning it off takes everything back but leaves providing as it is.
    void save(checked ? { ...g, Use: true, Tools: reads.map(d => d.name) } : { ...g, Use: false, Tools: [], Models: [], Capabilities: {} });
  }, t('usesHint'), [
    group(t('groupRead'), reads.map(tool)),
    group(t('groupChange'), changes.map(tool)),
    group(t('groupPage'), pages),
    group(t('groupModels'), models),
  ]);
}

function providesSection() {
  const mine = view!.pageTools.filter(d => d.origins?.includes(origin));
  const consumers = view!.grants.filter(g => g.Use && g.Site !== view!.site);
  const offers = (g: Grant, d: Definition) => { try { return new URL(g.Capabilities?.[d.name] ?? '').origin === origin; } catch { return false; } };
  const self = own();
  const on = Boolean(self.Provide);
  const body: (HTMLElement | null)[] = [];
  if (!mine.length) body.push(el('p', { className: 'empty' }, t('noPageTools')));
  else if (!consumers.length) body.push(el('p', { className: 'empty' }, t('noConsumers')));
  else for (const d of mine) {
    body.push(group(label(d), consumers.map(g => row(g.Site.replace(/^https?:\/\//, ''), d.description, offers(g, d), checked => {
      const capabilities = { ...(g.Capabilities ?? {}) };
      if (checked) capabilities[d.name] = page; else delete capabilities[d.name];
      void save({ ...g, Capabilities: capabilities });
    }))));
  }
  // The switch is the authorization, kept whether or not the site has tools yet; an AI may distil them later.
  return section('provides', t('providesTitle'), on, checked => save({ ...self, Provide: checked }), t('providesHint'), body);
}

function paintConnection() {
  const connection = $('connection');
  connection.textContent = t(view ? 'connected' : 'disconnected');
  connection.className = view ? '' : 'off';
  // The grant covers the whole site; the page's own host is shown beneath it when they differ.
  const host = origin.replace(/^https?:\/\//, ''), site = view?.site.replace(/^https?:\/\//, '') ?? host;
  $('site').replaceChildren(site || '—', ...(site && host !== site ? [el('small', {}, ` · ${host}`)] : []));
}

function render() {
  panel.replaceChildren();
  if (!view) return;
  if (!origin) { panel.append(el('p', { className: 'empty' }, t('notAWebsite'))); return; }
  panel.append(usesSection(), providesSection());
}

async function main() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  try {
    const url = new URL(tab?.url ?? '');
    if (url.protocol === 'https:' || url.protocol === 'http:') { origin = url.origin; page = url.href; }
  } catch { /* not a web page */ }
  await loadLanguage();
  try { view = await grants('get', origin); } catch { view = null; }
  paintConnection();
  render();
}

void main();
