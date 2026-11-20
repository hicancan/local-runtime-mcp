import { grants, type Definition, type Grant, type View } from './grants.js';

// The owner's panel for the site in the current tab: what it may use of this computer (blue) and which of its own page
// tools it offers to other sites (green). Every change goes to lrmcp at once and lands in its configuration file.
const t = (key: string, ...subs: string[]) => chrome.i18n.getMessage(key, subs) || key;
const $ = (id: string) => document.getElementById(id)!;
const status = $('status'), panel = $('panel');

for (const node of document.querySelectorAll<HTMLElement>('[data-i18n]')) node.textContent = t(node.dataset.i18n!);
document.documentElement.lang = chrome.i18n.getUILanguage();
$('settings').addEventListener('click', event => { event.preventDefault(); void chrome.runtime.openOptionsPage(); });

function el<K extends keyof HTMLElementTagNameMap>(tag: K, props: Record<string, unknown> = {}, ...children: (Node | string)[]): HTMLElementTagNameMap[K] {
  const node: HTMLElementTagNameMap[K] = document.createElement(tag);
  Object.assign(node, props);
  node.append(...children);
  return node;
}

/** A tool's name in the owner's language: the locale's translation of a built-in tool, else its own title. */
function label(d: Definition) {
  return chrome.i18n.getMessage(`tool_${d.name.replace(/\W/g, '_')}`) || d.title || d.name;
}

function row(text: string, title: string, checked: boolean, change: (on: boolean) => void) {
  const box = el('input', { type: 'checkbox', checked });
  box.addEventListener('change', () => change(box.checked));
  return el('label', { className: 'row', title }, box, el('span', {}, text));
}

function group(title: string, rows: HTMLElement[]) {
  return rows.length ? el('div', { className: 'group' }, el('h4', {}, title), ...rows) : null;
}

function section(kind: 'uses' | 'provides', title: string, on: boolean, toggle: (on: boolean) => void, hint: string, body: (HTMLElement | null)[]) {
  const master = el('input', { type: 'checkbox', checked: on });
  master.addEventListener('change', () => toggle(master.checked));
  const node = el('section', { className: kind }, el('label', {}, el('span', { className: 'dot' }), el('span', {}, title), master));
  if (on) node.append(el('p', { className: 'hint' }, hint), ...body.filter((n): n is HTMLElement => Boolean(n)));
  return node;
}

let view: View | null = null;
let origin = '', page = '';
let providesOpen = false;

async function save(grant: Grant) {
  try {
    view = await grants('set', grant);
    status.textContent = t('saved');
    void chrome.runtime.sendMessage({ type: 'grants-changed' }).catch(() => {});
  } catch (error) {
    status.textContent = t('failed', error instanceof Error ? error.message : String(error));
  }
  render();
}

function own(): Grant {
  const g = view!.grants.find(g => g.Origin === origin);
  return { Origin: origin, Models: [...(g?.Models ?? [])], Capabilities: { ...(g?.Capabilities ?? {}) }, Tools: [...(g?.Tools ?? [])] };
}

function flip(list: string[], item: string, on: boolean) {
  const rest = list.filter(x => x !== item);
  return on ? [...rest, item] : rest;
}

function usesSection() {
  const g = own();
  const on = (g.Models!.length + Object.keys(g.Capabilities!).length + g.Tools!.length) > 0;
  const tool = (d: Definition) => row(label(d), d.description, g.Tools!.includes(d.name), checked => save({ ...g, Tools: flip(g.Tools!, d.name, checked) }));
  const reads = view!.machine.filter(d => d.annotations.readOnlyHint);
  const changes = view!.machine.filter(d => !d.annotations.readOnlyHint);
  // Page tools another site offers, run on the page the owner chose when offering it.
  const offered = view!.pageTools.filter(d => !d.origins?.includes(origin));
  const pages = offered.map(d => row(label(d), d.description, d.name in g.Capabilities!, checked => {
    const capabilities = { ...g.Capabilities! };
    if (checked) capabilities[d.name] = d.origins?.[0] ? `${d.origins[0]}/` : ''; else delete capabilities[d.name];
    void save({ ...g, Capabilities: capabilities });
  }));
  const models = view!.models.map(m => row(m.title || m.id, m.id, g.Models!.includes(m.id), checked => save({ ...g, Models: flip(g.Models!, m.id, checked) })));
  return section('uses', t('usesTitle'), on, checked => {
    // Turning it on lends what only reads; anything that changes the computer stays one tick away.
    void save(checked ? { Origin: origin, Tools: reads.map(d => d.name), Models: [], Capabilities: {} } : { Origin: origin });
  }, t('usesHint'), [
    group(t('groupRead'), reads.map(tool)),
    group(t('groupChange'), changes.map(tool)),
    group(t('groupPage'), pages),
    group(t('groupModels'), models),
  ]);
}

function providesSection() {
  const mine = view!.pageTools.filter(d => d.origins?.includes(origin));
  const consumers = view!.grants.filter(g => g.Origin !== origin);
  const offers = (g: Grant, d: Definition) => { try { return new URL(g.Capabilities?.[d.name] ?? '').origin === origin; } catch { return false; } };
  const on = mine.some(d => consumers.some(g => offers(g, d)));
  const body: (HTMLElement | null)[] = [];
  if (!mine.length) body.push(el('p', { className: 'empty' }, t('noPageTools')));
  else if (!consumers.length) body.push(el('p', { className: 'empty' }, t('noConsumers')));
  else for (const d of mine) {
    body.push(group(label(d), consumers.map(g => row(g.Origin.replace(/^https?:\/\//, ''), d.description, offers(g, d), checked => {
      const capabilities = { ...(g.Capabilities ?? {}) };
      if (checked) capabilities[d.name] = page; else delete capabilities[d.name];
      void save({ ...g, Capabilities: capabilities });
    }))));
  }
  return section('provides', t('providesTitle'), on || providesOpen, checked => {
    providesOpen = checked;
    if (checked) { render(); return; }
    // Turning it off takes back every offer of this site's tools.
    void (async () => {
      for (const g of consumers) {
        const capabilities = Object.fromEntries(Object.entries(g.Capabilities ?? {}).filter(([name]) => !mine.some(d => d.name === name && offers(g, d))));
        if (Object.keys(capabilities).length !== Object.keys(g.Capabilities ?? {}).length) await save({ ...g, Capabilities: capabilities });
      }
    })();
  }, t('providesHint'), body);
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
  $('site').textContent = origin ? origin.replace(/^https?:\/\//, '') : '—';
  const connection = $('connection');
  try {
    view = await grants('get');
    connection.textContent = t('connected');
  } catch {
    connection.textContent = t('disconnected');
    connection.className = 'off';
  }
  render();
}

void main();
