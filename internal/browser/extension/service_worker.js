import { packagedConfig } from './runtime_config.js';

const VERSION = '5.0.0';
const attachedTabs = new Set();
const pageStates = new Map();
let generation = 0;
let stateSequence = 0;

chrome.runtime.onMessage.addListener((message) => {
  if (message?.type === 'settings-changed') run(++generation);
});
chrome.debugger.onDetach.addListener((source) => {
  attachedTabs.delete(source.tabId);
  invalidatePage(source.tabId);
});
chrome.tabs.onRemoved.addListener((tabId) => {
  attachedTabs.delete(tabId);
  invalidatePage(tabId);
});
chrome.tabs.onUpdated.addListener((tabId, change) => {
  if (change.status === 'loading' || change.url) invalidatePage(tabId);
});

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
    const headers = { Authorization: `Bearer ${settings.token}`, 'Content-Type': 'application/json' };
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
    case 'tabs.open': {
      const tab = await chrome.tabs.create({ url: params.url || 'about:blank', active: params.active !== false });
      await waitForLoad(tab.id);
      return tabView(await chrome.tabs.get(tab.id));
    }
    case 'tabs.close':
      invalidatePage(requireTabID(params));
      await chrome.tabs.remove(params.tab_id);
      return {};
    case 'page.navigate': {
      const id = requireTabID(params);
      invalidatePage(id);
      switch (params.kind) {
        case 'url':
          await chrome.tabs.update(id, { url: requireString(params.url, 'url') });
          break;
        case 'back':
          await chrome.tabs.goBack(id);
          break;
        case 'forward':
          await chrome.tabs.goForward(id);
          break;
        case 'reload':
          await chrome.tabs.reload(id);
          break;
        default:
          throw new Error(`unsupported browser navigation ${params.kind}`);
      }
      await waitForLoad(id);
      return tabView(await chrome.tabs.get(id));
    }
    case 'page.snapshot':
      return snapshot(requireTabID(params), params.max_elements || 500, params.max_text || 50000);
    case 'page.screenshot':
      return screenshot(requireTabID(params), params);
    case 'page.action':
      return action(params);
    default:
      throw new Error(`unsupported bridge method ${method}`);
  }
}

async function snapshot(tabId, maxElements, maxText) {
  const tab = await chrome.tabs.get(tabId);
  const state = pageState(tabId);
  const snapshotId = `${state.epoch}:q${(++stateSequence).toString(36)}`;
  const value = await callPage(tabId, pageSnapshotProjection, [
    snapshotId,
    numberLiteral(maxElements, 1, 5000),
    numberLiteral(maxText, 1, 500000)
  ]);
  return {
    tab_id: tabId, page_epoch: state.epoch, snapshot_id: value.snapshot_id, title: tab.title || '', url: tab.url || '',
    text: value.text, text_truncated: value.text_truncated,
    elements: value.elements, elements_truncated: value.elements_truncated
  };
}

function pageSnapshotProjection(snapshotId, maxElements, maxText) {
  const stored = Object.create(null), output = [], texts = [];
  let elementsTruncated = false;
  const interactive = element => element.matches('a,button,input,textarea,select,summary,[role],[contenteditable="true"],[tabindex]');
  const rendered = element => {
    const view = element.ownerDocument && element.ownerDocument.defaultView;
    if (!view) return false;
    const style = view.getComputedStyle(element), rect = element.getBoundingClientRect();
    return style.visibility !== 'hidden' && style.display !== 'none' && rect.width > 0 && rect.height > 0;
  };
  const accessibleName = element => {
    const labelledBy = (element.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean)
      .map(id => element.ownerDocument.getElementById(id)?.textContent || '').join(' ');
    return element.getAttribute('aria-label') || labelledBy || element.getAttribute('title') ||
      (element.labels ? Array.from(element.labels).map(label => label.innerText).join(' ') : '') || '';
  };
  const visitRoot = (root, offsetX, offsetY) => {
    const children = root.querySelectorAll ? Array.from(root.querySelectorAll('*')) : [];
    for (const element of children) {
      if (interactive(element) && rendered(element)) {
        if (output.length >= maxElements) {
          elementsTruncated = true;
        } else {
          const ref = snapshotId + ':e' + (output.length + 1);
          stored[ref] = element;
          const rect = element.getBoundingClientRect();
          output.push({
            ref, tag: element.tagName.toLowerCase(), role: element.getAttribute('role') || '',
            name: accessibleName(element).trim().slice(0, 500),
            text: (element.innerText || element.textContent || '').trim().slice(0, 500),
            href: String(element.href || '').slice(0, 4000), placeholder: String(element.placeholder || '').slice(0, 500),
            value: String(element.value || '').slice(0, 2000), disabled: !!element.disabled,
            rect: { x: Math.round(offsetX + rect.x), y: Math.round(offsetY + rect.y), width: Math.round(rect.width), height: Math.round(rect.height) }
          });
        }
      }
      if (element.shadowRoot) visitRoot(element.shadowRoot, offsetX, offsetY);
      if (element.tagName === 'IFRAME') {
        try {
          const frameDocument = element.contentDocument;
          if (frameDocument) {
            const frameRect = element.getBoundingClientRect();
            if (frameDocument.body?.innerText) texts.push(frameDocument.body.innerText);
            visitRoot(frameDocument, offsetX + frameRect.x, offsetY + frameRect.y);
          }
        } catch {}
      }
    }
  };
  texts.push(document.body?.innerText || '');
  visitRoot(document, 0, 0);
  const joinedText = texts.join('\n');
  const snapshots = globalThis.__lrmcpSnapshots || (globalThis.__lrmcpSnapshots = { order: [], values: Object.create(null) });
  snapshots.values[snapshotId] = stored;
  snapshots.order.push(snapshotId);
  while (snapshots.order.length > 8) delete snapshots.values[snapshots.order.shift()];
  return {
    snapshot_id: snapshotId, text: joinedText.slice(0, maxText), text_truncated: joinedText.length > maxText,
    elements: output, elements_truncated: elementsTruncated
  };
}

async function screenshot(tabId, params) {
  const tab = await chrome.tabs.get(tabId);
  const state = pageState(tabId);
  await attach(tabId);
  const metrics = await chrome.debugger.sendCommand({ tabId }, 'Page.getLayoutMetrics');
  const viewport = metrics.cssVisualViewport || metrics.visualViewport;
  const content = metrics.cssContentSize || metrics.contentSize;
  let x = 0, y = 0, width = Math.round(viewport.clientWidth), height = Math.round(viewport.clientHeight);
  let captureBeyondViewport = false;
  let clip;
  if (params.full_page) {
    width = Math.ceil(content.width); height = Math.ceil(content.height); captureBeyondViewport = true;
    clip = { x: 0, y: 0, width, height, scale: 1 };
  } else if (params.clip_width || params.clip_height) {
    x = params.clip_x || 0; y = params.clip_y || 0;
    width = params.clip_width; height = params.clip_height; captureBeyondViewport = true;
    clip = { x, y, width, height, scale: 1 };
  }
  if (width < 1 || height < 1 || width > 32768 || height > 32768 || width * height > 100000000) {
    throw new Error('requested screenshot dimensions are outside the supported bounds');
  }
  const options = { format: 'png', fromSurface: true, captureBeyondViewport };
  if (clip) options.clip = clip;
  const capture = await chrome.debugger.sendCommand({ tabId }, 'Page.captureScreenshot', options);
  if (capture.data.length > 60 * 1024 * 1024) throw new Error('encoded screenshot exceeds the 60 MiB bridge limit');
  let screenshotId = '';
  if (!params.full_page && !clip) {
    screenshotId = `${state.epoch}:p${(++stateSequence).toString(36)}`;
    state.screenshot = { id: screenshotId, width, height };
  }
  return {
    data_base64: capture.data,
    info: {
      tab_id: tabId, page_epoch: state.epoch, screenshot_id: screenshotId, title: tab.title || '', url: tab.url || '',
      x, y, width, height, full_page: !!params.full_page, mime_type: 'image/png'
    }
  };
}

async function action(params) {
  const tabId = requireTabID(params);
  validateActionState(tabId, params);
  let value;
  switch (params.kind) {
    case 'click':
    case 'double_click': {
      const point = await targetPoint(tabId, params);
      await clickAt(tabId, point, params.button || 'left', params.kind === 'double_click' ? 2 : 1);
      value = true;
      break;
    }
    case 'hover': {
      const point = await targetPoint(tabId, params);
      await mouse(tabId, 'mouseMoved', point.x, point.y, params.button || 'none', 0, 0);
      value = true;
      break;
    }
    case 'drag': {
      const start = await targetPoint(tabId, params);
      const end = { x: params.to_x, y: params.to_y };
      const button = params.button || 'left';
      const buttons = buttonMask(button);
      if (!Number.isFinite(end.x) || !Number.isFinite(end.y)) throw new Error('drag requires to_x and to_y');
      if (params.screenshot_id) {
        const viewport = pageStates.get(tabId)?.screenshot;
        if (!viewport || end.x < 0 || end.y < 0 || end.x >= viewport.width || end.y >= viewport.height) {
          throw new Error('drag destination is outside the referenced viewport screenshot');
        }
      }
      await mouse(tabId, 'mouseMoved', start.x, start.y, 'none', 0, 0);
      await mouse(tabId, 'mousePressed', start.x, start.y, button, buttons, 1);
      for (let step = 1; step <= 12; step++) {
        await mouse(tabId, 'mouseMoved', start.x + (end.x - start.x) * step / 12, start.y + (end.y - start.y) * step / 12, button, buttons, 0);
      }
      await mouse(tabId, 'mouseReleased', end.x, end.y, button, 0, 1);
      value = true;
      break;
    }
    case 'type_text':
    case 'set_value': {
      const point = await targetPoint(tabId, params);
      await clickAt(tabId, point, 'left', 1);
      if (params.kind === 'set_value') await dispatchKey(tabId, 'Control+A');
      await attach(tabId);
      await chrome.debugger.sendCommand({ tabId }, 'Input.insertText', { text: requireString(params.text, 'text') });
      value = true;
      break;
    }
    case 'press_key':
      if (params.selector || params.ref) await clickAt(tabId, await targetPoint(tabId, params), 'left', 1);
      await dispatchKey(tabId, requireString(params.key, 'key'));
      value = true;
      break;
    case 'scroll': {
      const point = params.selector || params.ref || params.screenshot_id
        ? await targetPoint(tabId, params)
        : await callPage(tabId, pageCenter, []);
      await mouse(tabId, 'mouseWheel', point.x, point.y, 'none', 0, 0, params.scroll_x || 0, params.scroll_y || 0);
      value = true;
      break;
    }
    case 'select':
      value = await callPage(tabId, pageSelect, [params]);
      break;
    case 'check':
      value = await callPage(tabId, pageCheck, [params]);
      break;
    case 'upload_files': {
      const objectId = await targetObject(tabId, params);
      try {
        await chrome.debugger.sendCommand({ tabId }, 'DOM.setFileInputFiles', { files: params.files, objectId });
      } finally {
        await chrome.debugger.sendCommand({ tabId }, 'Runtime.releaseObject', { objectId }).catch(() => {});
      }
      value = true;
      break;
    }
    case 'handle_dialog':
      await attach(tabId);
      await chrome.debugger.sendCommand({ tabId }, 'Page.handleJavaScriptDialog', { accept: params.accept === true, promptText: params.prompt_text || '' });
      value = true;
      break;
    case 'evaluate':
      value = await evaluate(tabId, requireString(params.script, 'script'));
      break;
    default:
      throw new Error(`unsupported browser action ${params.kind}`);
  }
  invalidatePage(tabId);
  return { tab_id: tabId, kind: params.kind, success: true, value };
}

async function targetPoint(tabId, params) {
  if (params.screenshot_id) {
    const state = pageStates.get(tabId)?.screenshot;
    if (!state || state.id !== params.screenshot_id) throw new Error('screenshot_id is stale; capture the viewport again');
    const x = Number(params.x), y = Number(params.y);
    if (!Number.isFinite(x) || !Number.isFinite(y) || x < 0 || y < 0 || x >= state.width || y >= state.height) {
      throw new Error('coordinates are outside the referenced viewport screenshot');
    }
    return { x, y };
  }
  return callPage(tabId, pageTargetPoint, [params]);
}

function pageTargetPoint(params) {
  let element;
  if (params.selector) {
    element = document.querySelector(params.selector);
  } else {
    const marker = params.ref.lastIndexOf(':e');
    const snapshotId = marker > 0 ? params.ref.slice(0, marker) : '';
    element = globalThis.__lrmcpSnapshots?.values?.[snapshotId]?.[params.ref];
  }
  if (!element || !element.isConnected) throw new Error('target element was not found or is stale');
  element.scrollIntoView({ block: 'center', inline: 'center' });
  const rect = element.getBoundingClientRect();
  let x = rect.x + rect.width / 2, y = rect.y + rect.height / 2, view = element.ownerDocument.defaultView;
  while (view && view !== top) {
    const frame = view.frameElement, frameRect = frame.getBoundingClientRect();
    x += frameRect.x; y += frameRect.y; view = frame.ownerDocument.defaultView;
  }
  return { x: Math.round(x), y: Math.round(y) };
}

function pageTargetObject(params) {
  let element;
  if (params.selector) {
    element = document.querySelector(params.selector);
  } else {
    const marker = params.ref.lastIndexOf(':e');
    const snapshotId = marker > 0 ? params.ref.slice(0, marker) : '';
    element = globalThis.__lrmcpSnapshots?.values?.[snapshotId]?.[params.ref];
  }
  if (!element || !element.isConnected) throw new Error('target element was not found or is stale');
  return element;
}

function pageSelect(params) {
  let element;
  if (params.selector) {
    element = document.querySelector(params.selector);
  } else {
    const marker = params.ref.lastIndexOf(':e');
    const snapshotId = marker > 0 ? params.ref.slice(0, marker) : '';
    element = globalThis.__lrmcpSnapshots?.values?.[snapshotId]?.[params.ref];
  }
  if (!(element instanceof HTMLSelectElement) || !element.isConnected) throw new Error('target is not a connected select element');
  const wanted = params.option || '';
  const option = Array.from(element.options).find(item => item.value === wanted || item.text === wanted || item.label === wanted);
  if (!option) throw new Error('select option was not found');
  element.value = option.value;
  element.dispatchEvent(new Event('input', { bubbles: true }));
  element.dispatchEvent(new Event('change', { bubbles: true }));
  return option.value;
}

function pageCheck(params) {
  let element;
  if (params.selector) {
    element = document.querySelector(params.selector);
  } else {
    const marker = params.ref.lastIndexOf(':e');
    const snapshotId = marker > 0 ? params.ref.slice(0, marker) : '';
    element = globalThis.__lrmcpSnapshots?.values?.[snapshotId]?.[params.ref];
  }
  if (!(element instanceof HTMLInputElement) || !['checkbox', 'radio'].includes(element.type) || !element.isConnected) {
    throw new Error('target is not a connected checkbox or radio');
  }
  const wanted = params.checked === true;
  if (element.checked !== wanted) element.click();
  return element.checked;
}

function pageCenter() {
  return { x: Math.round(innerWidth / 2), y: Math.round(innerHeight / 2) };
}

async function targetObject(tabId, params) {
  const result = await callPage(tabId, pageTargetObject, [params], false);
  if (!result?.objectId) throw new Error('target element did not produce a remote object');
  return result.objectId;
}

function pageState(tabId) {
  let state = pageStates.get(tabId);
  if (!state) {
    state = { epoch: `b${Date.now().toString(36)}-${(++stateSequence).toString(36)}`, screenshot: null };
    pageStates.set(tabId, state);
  }
  return state;
}

function invalidatePage(tabId) {
  pageStates.delete(tabId);
}

function validateActionState(tabId, params) {
  if (!params.ref && !params.screenshot_id) return;
  const state = pageStates.get(tabId);
  if (!state) throw new Error('page observation is stale; call browser_snapshot or browser_screenshot again');
  if (params.ref && !params.ref.startsWith(state.epoch + ':q')) {
    throw new Error('element ref is stale; call browser_snapshot again');
  }
  if (params.screenshot_id && state.screenshot?.id !== params.screenshot_id) {
    throw new Error('screenshot_id is stale; capture the viewport again');
  }
}

async function callPage(tabId, callback, args, returnByValue = true) {
  await attach(tabId);
  // Function#toString preserves escapes in the static callback source; only
  // JSON-encoded data is appended. This avoids hand-built nested page scripts.
  const expression = `(${callback.toString()}).apply(globalThis, ${JSON.stringify(args)})`;
  const response = await chrome.debugger.sendCommand({ tabId }, 'Runtime.evaluate', {
    expression, awaitPromise: true, returnByValue, userGesture: true
  });
  if (response.exceptionDetails) throw new Error(exceptionMessage(response));
  return returnByValue ? response.result?.value : response.result;
}

async function clickAt(tabId, point, button, count) {
  await mouse(tabId, 'mouseMoved', point.x, point.y, 'none', 0, 0);
  await mouse(tabId, 'mousePressed', point.x, point.y, button, buttonMask(button), count);
  await mouse(tabId, 'mouseReleased', point.x, point.y, button, 0, count);
}

async function mouse(tabId, type, x, y, button = 'none', buttons = 0, clickCount = 0, deltaX = 0, deltaY = 0) {
  await attach(tabId);
  await chrome.debugger.sendCommand({ tabId }, 'Input.dispatchMouseEvent', { type, x, y, button, buttons, clickCount, deltaX, deltaY });
}

function buttonMask(button) {
  return button === 'right' ? 2 : button === 'middle' ? 4 : 1;
}

async function dispatchKey(tabId, combination) {
  await attach(tabId);
  const parts = combination.split('+').map(value => value.trim()).filter(Boolean);
  const key = parts.pop();
  if (!key) throw new Error('key is required');
  const modifierValues = { alt: 1, control: 2, ctrl: 2, meta: 4, command: 4, shift: 8 };
  const modifiers = parts.reduce((value, part) => value | (modifierValues[part.toLowerCase()] || 0), 0);
  const info = keyInfo(key);
  const payload = { key: info.key, code: info.code, windowsVirtualKeyCode: info.codeValue, nativeVirtualKeyCode: info.codeValue, modifiers };
  await chrome.debugger.sendCommand({ tabId }, 'Input.dispatchKeyEvent', { ...payload, type: 'rawKeyDown' });
  await chrome.debugger.sendCommand({ tabId }, 'Input.dispatchKeyEvent', { ...payload, type: 'keyUp' });
}

function keyInfo(value) {
  const names = {
    Enter: ['Enter', 'Enter', 13], Tab: ['Tab', 'Tab', 9], Escape: ['Escape', 'Escape', 27], Esc: ['Escape', 'Escape', 27],
    Backspace: ['Backspace', 'Backspace', 8], Delete: ['Delete', 'Delete', 46], Space: [' ', 'Space', 32],
    ArrowLeft: ['ArrowLeft', 'ArrowLeft', 37], Left: ['ArrowLeft', 'ArrowLeft', 37], ArrowUp: ['ArrowUp', 'ArrowUp', 38], Up: ['ArrowUp', 'ArrowUp', 38],
    ArrowRight: ['ArrowRight', 'ArrowRight', 39], Right: ['ArrowRight', 'ArrowRight', 39], ArrowDown: ['ArrowDown', 'ArrowDown', 40], Down: ['ArrowDown', 'ArrowDown', 40],
    Home: ['Home', 'Home', 36], End: ['End', 'End', 35], PageUp: ['PageUp', 'PageUp', 33], PageDown: ['PageDown', 'PageDown', 34]
  };
  const named = names[value];
  if (named) return { key: named[0], code: named[1], codeValue: named[2] };
  if (/^F([1-9]|1[0-2])$/.test(value)) return { key: value, code: value, codeValue: 111 + Number(value.slice(1)) };
  if (value.length === 1) {
    const upper = value.toUpperCase();
    return { key: value, code: /[A-Z]/.test(upper) ? `Key${upper}` : /[0-9]/.test(value) ? `Digit${value}` : value, codeValue: upper.charCodeAt(0) };
  }
  return { key: value, code: value, codeValue: 0 };
}

async function evaluate(tabId, expression) {
  await attach(tabId);
  const response = await chrome.debugger.sendCommand({ tabId }, 'Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true, userGesture: true });
  if (response.exceptionDetails) throw new Error(exceptionMessage(response));
  return response.result?.value;
}

function exceptionMessage(response) {
  return response.exceptionDetails?.exception?.description || response.exceptionDetails?.text || 'JavaScript evaluation failed';
}

async function attach(tabId) {
  if (attachedTabs.has(tabId)) return;
  try {
    await chrome.debugger.attach({ tabId }, '1.3');
  } catch (error) {
    if (!String(error?.message || error).includes('already attached')) throw error;
  }
  attachedTabs.add(tabId);
}

async function waitForLoad(tabId) {
  const deadline = Date.now() + 30000;
  await delay(50);
  while (Date.now() < deadline) {
    const tab = await chrome.tabs.get(tabId);
    if (tab.status === 'complete') return;
    await delay(100);
  }
  throw new Error('browser navigation timed out after 30 seconds');
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
