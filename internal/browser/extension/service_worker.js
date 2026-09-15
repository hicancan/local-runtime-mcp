import { packagedConfig } from './runtime_config.js';
const VERSION = '6.0.0';
const attachedTabs = new Set();
const childSessions = new Map();
const pageStates = new Map();
let generation = 0;
let stateSequence = 0;
async function sendCDP(source, method, params) {
    return chrome.debugger.sendCommand(source, method, params);
}
chrome.runtime.onMessage.addListener((message) => {
    if (message?.type === 'settings-changed')
        run(++generation);
});
chrome.debugger.onDetach.addListener((source) => {
    if (!source.tabId)
        return;
    attachedTabs.delete(source.tabId);
    childSessions.delete(source.tabId);
    invalidatePage(source.tabId);
});
chrome.debugger.onEvent.addListener((source, method, rawParams) => {
    if (!source.tabId)
        return;
    const params = rawParams || {};
    if (method === 'Target.attachedToTarget' && params.sessionId) {
        let sessions = childSessions.get(source.tabId);
        if (!sessions)
            childSessions.set(source.tabId, sessions = new Set());
        sessions.add(params.sessionId);
        sendCDP({ tabId: source.tabId, sessionId: params.sessionId }, 'Target.setAutoAttach', {
            autoAttach: true, waitForDebuggerOnStart: false, flatten: true,
            filter: [{ type: 'iframe', exclude: false }]
        }).catch(() => { });
    }
    else if (method === 'Target.detachedFromTarget' && params.sessionId) {
        childSessions.get(source.tabId)?.delete(params.sessionId);
    }
});
chrome.tabs.onRemoved.addListener((tabId) => {
    attachedTabs.delete(tabId);
    invalidatePage(tabId);
});
chrome.tabs.onUpdated.addListener((tabId, change) => {
    if (change.status === 'loading' || change.url)
        invalidatePage(tabId);
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
            if (response.status === 204)
                continue;
            if (!response.ok)
                throw new Error(`bridge returned HTTP ${response.status}`);
            const command = await response.json();
            let payload;
            try {
                payload = { id: command.id, instance_id: settings.instanceId, result: await execute(command.method, command.params || {}) };
            }
            catch (error) {
                payload = { id: command.id, instance_id: settings.instanceId, error: error?.message || String(error) };
            }
            const sent = await fetch(`${base}/result`, { method: 'POST', headers, body: JSON.stringify(payload) });
            if (!sent.ok && sent.status !== 404)
                throw new Error(`result returned HTTP ${sent.status}`);
        }
        catch {
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
            if (!tab.id)
                throw new Error('browser did not assign a tab ID');
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
    await attach(tabId);
    // Auto-attach events for already existing OOPIF targets are asynchronous.
    await delay(50);
    const limit = numberLiteral(maxElements, 1, 5000), textLimit = numberLiteral(maxText, 1, 500000);
    const sources = [{ tabId }, ...Array.from(childSessions.get(tabId) || []).map(sessionId => ({ tabId, sessionId }))];
    const elements = [], texts = [], refs = new Map();
    let elementsTruncated = false;
    for (const source of sources) {
        let tree;
        try {
            tree = await sendCDP(source, 'Accessibility.getFullAXTree');
        }
        catch {
            continue;
        }
        for (const node of tree.nodes || []) {
            if (node.ignored)
                continue;
            const role = String(node.role?.value || ''), name = String(node.name?.value || ''), value = String(node.value?.value || '');
            if (name)
                texts.push(name);
            if (value && value !== name)
                texts.push(value);
            if (!node.backendDOMNodeId || !interactiveAXRoles.has(role))
                continue;
            if (elements.length >= limit) {
                elementsTruncated = true;
                continue;
            }
            const ref = `${snapshotId}:e${elements.length + 1}`;
            const properties = Object.fromEntries((node.properties || []).map((item) => [item.name, item.value?.value]));
            refs.set(ref, { source, backendNodeId: node.backendDOMNodeId });
            elements.push({
                ref, role, name: name.slice(0, 500),
                value: value.slice(0, 2000), disabled: properties.disabled === true
            });
        }
    }
    state.snapshots.set(snapshotId, refs);
    state.snapshotOrder.push(snapshotId);
    while (state.snapshotOrder.length > 8) {
        const expired = state.snapshotOrder.shift();
        if (expired)
            state.snapshots.delete(expired);
    }
    const joinedText = texts.join('\n');
    return {
        tab_id: tabId, page_epoch: state.epoch, snapshot_id: snapshotId, title: tab.title || '', url: tab.url || '',
        text: joinedText.slice(0, textLimit), text_truncated: joinedText.length > textLimit,
        elements, elements_truncated: elementsTruncated
    };
}
const interactiveAXRoles = new Set([
    'button', 'link', 'textbox', 'searchbox', 'combobox', 'listbox', 'option', 'checkbox', 'radio',
    'switch', 'slider', 'spinbutton', 'menuitem', 'menuitemcheckbox', 'menuitemradio', 'tab', 'treeitem'
]);
async function screenshot(tabId, params) {
    const tab = await chrome.tabs.get(tabId);
    const state = pageState(tabId);
    await attach(tabId);
    const metrics = await sendCDP({ tabId }, 'Page.getLayoutMetrics');
    const viewport = metrics.cssVisualViewport || metrics.visualViewport;
    const content = metrics.cssContentSize || metrics.contentSize;
    let x = 0, y = 0, width = Math.round(viewport.clientWidth), height = Math.round(viewport.clientHeight);
    let captureBeyondViewport = false;
    let clip;
    if (params.full_page) {
        width = Math.ceil(content.width);
        height = Math.ceil(content.height);
        captureBeyondViewport = true;
        clip = { x: 0, y: 0, width, height, scale: 1 };
    }
    else if (params.clip_width || params.clip_height) {
        x = params.clip_x || 0;
        y = params.clip_y || 0;
        width = params.clip_width;
        height = params.clip_height;
        captureBeyondViewport = true;
        clip = { x, y, width, height, scale: 1 };
    }
    if (width < 1 || height < 1 || width > 32768 || height > 32768 || width * height > 100000000) {
        throw new Error('requested screenshot dimensions are outside the supported bounds');
    }
    const options = { format: 'png', fromSurface: true, captureBeyondViewport };
    if (clip)
        options.clip = clip;
    const capture = await sendCDP({ tabId }, 'Page.captureScreenshot', options);
    if (capture.data.length > 60 * 1024 * 1024)
        throw new Error('encoded screenshot exceeds the 60 MiB bridge limit');
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
            await clickAt(point, params.button || 'left', params.kind === 'double_click' ? 2 : 1);
            value = true;
            break;
        }
        case 'hover': {
            const point = await targetPoint(tabId, params);
            await mouse(point.source, 'mouseMoved', point.x, point.y, params.button || 'none', 0, 0);
            value = true;
            break;
        }
        case 'drag': {
            const start = await targetPoint(tabId, params);
            const end = { x: params.to_x, y: params.to_y };
            const button = params.button || 'left';
            const buttons = buttonMask(button);
            if (!Number.isFinite(end.x) || !Number.isFinite(end.y))
                throw new Error('drag requires to_x and to_y');
            if (params.screenshot_id) {
                const viewport = pageStates.get(tabId)?.screenshot;
                if (!viewport || end.x < 0 || end.y < 0 || end.x >= viewport.width || end.y >= viewport.height) {
                    throw new Error('drag destination is outside the referenced viewport screenshot');
                }
            }
            await mouse(start.source, 'mouseMoved', start.x, start.y, 'none', 0, 0);
            await mouse(start.source, 'mousePressed', start.x, start.y, button, buttons, 1);
            for (let step = 1; step <= 12; step++) {
                await mouse(start.source, 'mouseMoved', start.x + (end.x - start.x) * step / 12, start.y + (end.y - start.y) * step / 12, button, buttons, 0);
            }
            await mouse(start.source, 'mouseReleased', end.x, end.y, button, 0, 1);
            value = true;
            break;
        }
        case 'type_text':
        case 'set_value': {
            const point = await targetPoint(tabId, params);
            await clickAt(point, 'left', 1);
            if (params.kind === 'set_value')
                await dispatchKey(tabId, 'Control+A');
            await sendCDP(point.source, 'Input.insertText', { text: requireString(params.text, 'text') });
            value = true;
            break;
        }
        case 'press_key':
            if (params.ref)
                await clickAt(await targetPoint(tabId, params), 'left', 1);
            await dispatchKey(tabId, requireString(params.key, 'key'));
            value = true;
            break;
        case 'scroll': {
            const point = params.ref || params.screenshot_id
                ? await targetPoint(tabId, params)
                : { ...(await callPage(tabId, pageCenter, [])), source: { tabId } };
            await mouse(point.source, 'mouseWheel', point.x, point.y, 'none', 0, 0, params.scroll_x || 0, params.scroll_y || 0);
            value = true;
            break;
        }
        case 'select':
            value = await callOnTarget(tabId, params.ref, function (option) {
                if (!(this instanceof HTMLSelectElement))
                    throw new Error('target is not a select element');
                const item = Array.from(this.options).find(entry => entry.value === option || entry.text === option || entry.label === option);
                if (!item)
                    throw new Error('select option was not found');
                this.value = item.value;
                this.dispatchEvent(new Event('input', { bubbles: true }));
                this.dispatchEvent(new Event('change', { bubbles: true }));
                return item.value;
            }, [params.option]);
            break;
        case 'check':
            value = await callOnTarget(tabId, params.ref, function (checked) {
                if (!(this instanceof HTMLInputElement) || !['checkbox', 'radio'].includes(this.type))
                    throw new Error('target is not a checkbox or radio');
                if (this.checked !== checked)
                    this.click();
                return this.checked;
            }, [params.checked === true]);
            break;
        case 'upload_files': {
            const target = targetRef(tabId, params.ref);
            await sendCDP(target.source, 'DOM.setFileInputFiles', { files: params.files, backendNodeId: target.backendNodeId });
            value = true;
            break;
        }
        case 'handle_dialog':
            await attach(tabId);
            await sendCDP({ tabId }, 'Page.handleJavaScriptDialog', { accept: params.accept === true, promptText: params.prompt_text || '' });
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
        if (!state || state.id !== params.screenshot_id)
            throw new Error('screenshot_id is stale; capture the viewport again');
        const x = Number(params.x), y = Number(params.y);
        if (!Number.isFinite(x) || !Number.isFinite(y) || x < 0 || y < 0 || x >= state.width || y >= state.height) {
            throw new Error('coordinates are outside the referenced viewport screenshot');
        }
        return { x, y, source: { tabId } };
    }
    const target = targetRef(tabId, params.ref);
    await sendCDP(target.source, 'DOM.scrollIntoViewIfNeeded', { backendNodeId: target.backendNodeId });
    const model = await sendCDP(target.source, 'DOM.getBoxModel', { backendNodeId: target.backendNodeId });
    const quad = model.model?.content || model.model?.border;
    if (!quad || quad.length !== 8)
        throw new Error('target element has no visible box');
    return { x: Math.round((quad[0] + quad[2] + quad[4] + quad[6]) / 4), y: Math.round((quad[1] + quad[3] + quad[5] + quad[7]) / 4), source: target.source };
}
function pageCenter() {
    return { x: Math.round(innerWidth / 2), y: Math.round(innerHeight / 2) };
}
function targetRef(tabId, ref) {
    if (!ref)
        throw new Error('element ref is required');
    const marker = ref.lastIndexOf(':e'), snapshotId = marker > 0 ? ref.slice(0, marker) : '';
    const target = pageStates.get(tabId)?.snapshots?.get(snapshotId)?.get(ref);
    if (!target)
        throw new Error('element ref was not found or is stale');
    return target;
}
async function callOnTarget(tabId, ref, callback, args) {
    const target = targetRef(tabId, ref);
    const resolved = await sendCDP(target.source, 'DOM.resolveNode', { backendNodeId: target.backendNodeId });
    const objectId = resolved.object?.objectId;
    if (!objectId)
        throw new Error('target element did not produce a remote object');
    try {
        const response = await sendCDP(target.source, 'Runtime.callFunctionOn', {
            objectId, functionDeclaration: callback.toString(), arguments: args.map(value => ({ value })), returnByValue: true, userGesture: true
        });
        if (response.exceptionDetails)
            throw new Error(exceptionMessage(response));
        return response.result?.value;
    }
    finally {
        await sendCDP(target.source, 'Runtime.releaseObject', { objectId }).catch(() => { });
    }
}
function pageState(tabId) {
    let state = pageStates.get(tabId);
    if (!state) {
        state = { epoch: `b${Date.now().toString(36)}-${(++stateSequence).toString(36)}`, screenshot: null, snapshots: new Map(), snapshotOrder: [] };
        pageStates.set(tabId, state);
    }
    return state;
}
function invalidatePage(tabId) {
    pageStates.delete(tabId);
}
function validateActionState(tabId, params) {
    if (!params.ref && !params.screenshot_id)
        return;
    const state = pageStates.get(tabId);
    if (!state)
        throw new Error('page observation is stale; call browser_snapshot or browser_screenshot again');
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
    const response = await sendCDP({ tabId }, 'Runtime.evaluate', {
        expression, awaitPromise: true, returnByValue, userGesture: true
    });
    if (response.exceptionDetails)
        throw new Error(exceptionMessage(response));
    return returnByValue ? response.result?.value : response.result;
}
async function clickAt(point, button, count) {
    await mouse(point.source, 'mouseMoved', point.x, point.y, 'none', 0, 0);
    await mouse(point.source, 'mousePressed', point.x, point.y, button, buttonMask(button), count);
    await mouse(point.source, 'mouseReleased', point.x, point.y, button, 0, count);
}
async function mouse(source, type, x, y, button = 'none', buttons = 0, clickCount = 0, deltaX = 0, deltaY = 0) {
    await attach(source.tabId);
    await sendCDP(source, 'Input.dispatchMouseEvent', { type, x, y, button, buttons, clickCount, deltaX, deltaY });
}
function buttonMask(button) {
    return button === 'right' ? 2 : button === 'middle' ? 4 : 1;
}
async function dispatchKey(tabId, combination) {
    await attach(tabId);
    const parts = combination.split('+').map(value => value.trim()).filter(Boolean);
    const key = parts.pop();
    if (!key)
        throw new Error('key is required');
    const modifierValues = { alt: 1, control: 2, ctrl: 2, meta: 4, command: 4, shift: 8 };
    const modifiers = parts.reduce((value, part) => value | (modifierValues[part.toLowerCase()] || 0), 0);
    const info = keyInfo(key);
    const payload = { key: info.key, code: info.code, windowsVirtualKeyCode: info.codeValue, nativeVirtualKeyCode: info.codeValue, modifiers };
    await sendCDP({ tabId }, 'Input.dispatchKeyEvent', { ...payload, type: 'rawKeyDown' });
    await sendCDP({ tabId }, 'Input.dispatchKeyEvent', { ...payload, type: 'keyUp' });
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
    if (named)
        return { key: named[0], code: named[1], codeValue: named[2] };
    if (/^F([1-9]|1[0-2])$/.test(value))
        return { key: value, code: value, codeValue: 111 + Number(value.slice(1)) };
    if (value.length === 1) {
        const upper = value.toUpperCase();
        return { key: value, code: /[A-Z]/.test(upper) ? `Key${upper}` : /[0-9]/.test(value) ? `Digit${value}` : value, codeValue: upper.charCodeAt(0) };
    }
    return { key: value, code: value, codeValue: 0 };
}
async function evaluate(tabId, expression) {
    await attach(tabId);
    const response = await sendCDP({ tabId }, 'Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true, userGesture: true });
    if (response.exceptionDetails)
        throw new Error(exceptionMessage(response));
    return response.result?.value;
}
function exceptionMessage(response) {
    return response.exceptionDetails?.exception?.description || response.exceptionDetails?.text || 'JavaScript evaluation failed';
}
async function attach(tabId) {
    if (attachedTabs.has(tabId))
        return;
    try {
        await chrome.debugger.attach({ tabId }, '1.3');
    }
    catch (error) {
        if (!String(error?.message || error).includes('already attached'))
            throw error;
    }
    attachedTabs.add(tabId);
    childSessions.set(tabId, new Set());
    await sendCDP({ tabId }, 'Target.setAutoAttach', {
        autoAttach: true, waitForDebuggerOnStart: false, flatten: true,
        filter: [{ type: 'iframe', exclude: false }]
    });
}
async function waitForLoad(tabId) {
    const deadline = Date.now() + 30000;
    await delay(50);
    while (Date.now() < deadline) {
        const tab = await chrome.tabs.get(tabId);
        if (tab.status === 'complete')
            return;
        await delay(100);
    }
    throw new Error('browser navigation timed out after 30 seconds');
}
function tabView(tab) {
    return { id: tab.id, window_id: tab.windowId, title: tab.title || '', url: tab.url || '', active: !!tab.active, status: tab.status || '' };
}
function isControllableTab(tab) {
    return (tab.id || 0) > 0 && !/^(chrome|edge|devtools|chrome-extension):/.test(tab.url || '');
}
function requireTabID(params) {
    if (!Number.isInteger(params.tab_id) || params.tab_id <= 0)
        throw new Error('tab_id must be a positive integer');
    return params.tab_id;
}
function requireString(value, name) {
    if (typeof value !== 'string' || !value)
        throw new Error(`${name} is required`);
    return value;
}
function numberLiteral(value, minimum, maximum) {
    const numeric = Number(value);
    return Number.isFinite(numeric) ? Math.max(minimum, Math.min(maximum, Math.trunc(numeric))) : minimum;
}
function delay(milliseconds) { return new Promise(resolve => setTimeout(resolve, milliseconds)); }
