import { packagedConfig } from './runtime_config.js';

const VERSION = '3.0.0';
const attachedTabs = new Set();
let generation = 0;

chrome.runtime.onInstalled.addListener(() => run(++generation));
chrome.runtime.onStartup.addListener(() => run(++generation));
chrome.runtime.onMessage.addListener((message) => {
  if (message?.type === 'settings-changed') run(++generation);
});
chrome.debugger.onDetach.addListener((source) => attachedTabs.delete(source.tabId));

run(++generation);

async function run(currentGeneration) {
  while (currentGeneration === generation) {
    const settings = await chrome.storage.local.get({
      address: packagedConfig.address || '127.0.0.1:9315',
      token: packagedConfig.token || '',
      instanceId: ''
    });
    if (!settings.instanceId) {
      settings.instanceId = crypto.randomUUID();
      await chrome.storage.local.set({ instanceId: settings.instanceId });
    }
    if (!settings.token || settings.token.length < 32) {
      await delay(2000);
      continue;
    }
    const base = `http://${settings.address}/v1`;
    const headers = { 'Authorization': `Bearer ${settings.token}`, 'Content-Type': 'application/json' };
    try {
      const response = await fetch(`${base}/poll`, {
        method: 'POST', headers,
        body: JSON.stringify({ instance_id: settings.instanceId, browser: navigator.userAgent, extension_version: VERSION })
      });
      if (response.status === 204) continue;
      if (!response.ok) throw new Error(`bridge returned HTTP ${response.status}`);
      const command = await response.json();
      let payload;
      try {
        payload = { id: command.id, instance_id: settings.instanceId, result: await execute(command.method, command.params || {}) };
      } catch (error) {
        payload = { id: command.id, instance_id: settings.instanceId, error: error?.message || String(error) };
      }
      const sent = await fetch(`${base}/result`, { method: 'POST', headers, body: JSON.stringify(payload) });
      if (!sent.ok && sent.status !== 404) throw new Error(`result returned HTTP ${sent.status}`);
    } catch {
      await delay(1000);
    }
  }
}

async function execute(method, params) {
  switch (method) {
    case 'tabs.list':
      return (await chrome.tabs.query({})).filter(isControllableTab).map(tabView);
    case 'tabs.open':
      return tabView(await chrome.tabs.create({ url: params.url || 'about:blank', active: params.active !== false }));
    case 'tabs.close':
      await chrome.tabs.remove(requireTabID(params));
      return {};
    case 'page.navigate': {
      const id = requireTabID(params);
      const tab = await chrome.tabs.update(id, { url: requireString(params.url, 'url') });
      return tabView(tab);
    }
    case 'page.snapshot':
      return snapshot(requireTabID(params), params.max_elements || 500, params.max_text || 50000);
    case 'page.screenshot':
      return screenshot(requireTabID(params));
    case 'page.action':
      return action(params);
    default:
      throw new Error(`unsupported bridge method ${method}`);
  }
}

async function snapshot(tabId, maxElements, maxText) {
  const tab = await chrome.tabs.get(tabId);
  const value = await evaluate(tabId, `(() => {
    const visible = element => {
      const style = getComputedStyle(element); const rect = element.getBoundingClientRect();
      return style.visibility !== 'hidden' && style.display !== 'none' && rect.width > 0 && rect.height > 0;
    };
    const selector = 'a,button,input,textarea,select,[role],[contenteditable="true"],[tabindex]';
    const elements = Array.from(document.querySelectorAll(selector)).filter(visible).slice(0, ${numberLiteral(maxElements, 1, 5000)}).map((element, index) => {
      const ref = 'r' + (index + 1); element.setAttribute('data-lrmcp-ref', ref);
      const rect = element.getBoundingClientRect();
      return {
        ref, tag: element.tagName.toLowerCase(), role: element.getAttribute('role') || '',
        name: element.getAttribute('aria-label') || element.getAttribute('title') || '',
        text: (element.innerText || element.textContent || '').trim().slice(0, 500),
        href: element.href || '', placeholder: element.placeholder || '', value: element.value || '',
        disabled: !!element.disabled,
        rect: { x: Math.round(rect.x), y: Math.round(rect.y), width: Math.round(rect.width), height: Math.round(rect.height) }
      };
    });
    return { text: (document.body?.innerText || '').slice(0, ${numberLiteral(maxText, 1, 500000)}), elements };
  })()`);
  return { tab_id: tabId, title: tab.title || '', url: tab.url || '', text: value.text, elements: value.elements };
}

async function screenshot(tabId) {
  const tab = await chrome.tabs.get(tabId);
  const metrics = await evaluate(tabId, `({ width: window.innerWidth, height: window.innerHeight })`);
  await attach(tabId);
  const capture = await chrome.debugger.sendCommand({ tabId }, 'Page.captureScreenshot', { format: 'png', fromSurface: true, captureBeyondViewport: false });
  return { data_base64: capture.data, info: { tab_id: tabId, title: tab.title || '', url: tab.url || '', width: metrics.width, height: metrics.height, mime_type: 'image/png' } };
}

async function action(params) {
  const tabId = requireTabID(params);
  let value;
  switch (params.kind) {
    case 'click':
      value = await evaluate(tabId, elementScript(params, `element.scrollIntoView({block:'center',inline:'center'}); element.click(); return true;`));
      break;
    case 'type':
      value = await evaluate(tabId, elementScript(params, `element.focus(); const text=${JSON.stringify(params.text || '')}; if (element.isContentEditable) element.textContent=text; else { const setter=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(element),'value')?.set; if (setter) setter.call(element,text); else element.value=text; } element.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'insertText',data:text})); element.dispatchEvent(new Event('change',{bubbles:true})); return true;`));
      break;
    case 'key':
      await dispatchKey(tabId, requireString(params.key, 'key'));
      value = true;
      break;
    case 'scroll':
      value = await evaluate(tabId, `window.scrollBy(${numberLiteral(params.scroll_x || 0, -1000000, 1000000)},${numberLiteral(params.scroll_y || 0, -1000000, 1000000)}); true`);
      break;
    case 'evaluate':
      value = await evaluate(tabId, requireString(params.script, 'script'));
      break;
    case 'back':
      await chrome.tabs.goBack(tabId); value = true; break;
    case 'forward':
      await chrome.tabs.goForward(tabId); value = true; break;
    case 'reload':
      await chrome.tabs.reload(tabId); value = true; break;
    default:
      throw new Error(`unsupported browser action ${params.kind}`);
  }
  return { tab_id: tabId, kind: params.kind, success: true, value };
}

function elementScript(params, operation) {
  const lookup = params.selector
    ? `document.querySelector(${JSON.stringify(params.selector)})`
    : `document.querySelector('[data-lrmcp-ref="'+CSS.escape(${JSON.stringify(params.ref || '')})+'"]')`;
  return `(() => { const element=${lookup}; if (!element) throw new Error('target element not found'); ${operation} })()`;
}

async function dispatchKey(tabId, combination) {
  await attach(tabId);
  const parts = combination.split('+').map(value => value.trim()).filter(Boolean);
  const key = parts.pop();
  const modifierValues = { alt: 1, control: 2, ctrl: 2, meta: 4, command: 4, shift: 8 };
  const modifiers = parts.reduce((value, part) => value | (modifierValues[part.toLowerCase()] || 0), 0);
  await chrome.debugger.sendCommand({ tabId }, 'Input.dispatchKeyEvent', { type: 'keyDown', key, code: key, modifiers });
  await chrome.debugger.sendCommand({ tabId }, 'Input.dispatchKeyEvent', { type: 'keyUp', key, code: key, modifiers });
}

async function evaluate(tabId, expression) {
  await attach(tabId);
  const response = await chrome.debugger.sendCommand({ tabId }, 'Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true, userGesture: true });
  if (response.exceptionDetails) throw new Error(response.exceptionDetails.exception?.description || response.exceptionDetails.text || 'JavaScript evaluation failed');
  return response.result?.value;
}

async function attach(tabId) {
  if (attachedTabs.has(tabId)) return;
  try {
    await chrome.debugger.attach({ tabId }, '1.3');
    attachedTabs.add(tabId);
  } catch (error) {
    if (!String(error?.message || error).includes('already attached')) throw error;
    attachedTabs.add(tabId);
  }
}

function tabView(tab) {
  return { id: tab.id, window_id: tab.windowId, title: tab.title || '', url: tab.url || '', active: !!tab.active, status: tab.status || '' };
}

function isControllableTab(tab) {
  return tab.id > 0 && !/^(chrome|edge|devtools|chrome-extension):/.test(tab.url || '');
}

function requireTabID(params) {
  if (!Number.isInteger(params.tab_id) || params.tab_id <= 0) throw new Error('tab_id must be a positive integer');
  return params.tab_id;
}

function requireString(value, name) {
  if (typeof value !== 'string' || !value) throw new Error(`${name} is required`);
  return value;
}

function numberLiteral(value, minimum, maximum) {
  const numeric = Number(value);
  return Number.isFinite(numeric) ? Math.max(minimum, Math.min(maximum, Math.trunc(numeric))) : minimum;
}

function delay(milliseconds) { return new Promise(resolve => setTimeout(resolve, milliseconds)); }
