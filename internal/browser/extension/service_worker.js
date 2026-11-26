import { packagedConfig } from './runtime_config.js';
import { commandStream } from './connection.js';
import { VERSION } from './version.js';
import { installWebBridge } from './web_bridge.js';
import { grants, roles } from './grants.js';
import { paintIcon } from './icon.js';
let generation = 0;
let bootId = crypto.randomUUID();
const tabHandles = new Map();
let connectionAbort = new AbortController();
let running = false;
let browserConnected = false;
let settingsRead;
function bridgeSettings() {
    if (!settingsRead)
        settingsRead = (async () => {
            const stored = await chrome.storage.local.get({ address: '', token: '', instanceId: '', label: '' });
            if (!stored.instanceId) {
                stored.instanceId = crypto.randomUUID();
                await chrome.storage.local.set({ instanceId: stored.instanceId });
            }
            const address = stored.address || packagedConfig.address || '127.0.0.1:9315';
            const token = stored.token || packagedConfig.token || '';
            return { address, token, instanceId: stored.instanceId, label: stored.label };
        })().finally(() => { settingsRead = undefined; });
    return settingsRead;
}
installWebBridge(async () => {
    const settings = await bridgeSettings();
    const deadline = Date.now() + 5000;
    while (!browserConnected && Date.now() < deadline)
        await delay(25);
    if (!browserConnected)
        throw new Error('local runtime browser connection is not ready');
    return { address: settings.address, token: settings.token, instanceId: settings.instanceId, bootId };
}, tabHandle);
chrome.runtime.onMessage.addListener((message) => {
    if (message?.type === 'grants-changed') {
        void refreshIcons();
        return;
    }
    if (message?.type === 'settings-changed') {
        browserConnected = false;
        connectionAbort.abort();
        bootId = crypto.randomUUID();
        tabHandles.clear();
        connectionAbort = new AbortController();
        void run(++generation);
    }
});
chrome.tabs.onRemoved.addListener((tabId) => { tabHandles.delete(tabId); });
chrome.tabs.onUpdated.addListener((_tabId, change, tab) => { if (change.url)
    void paintTab(tab); });
chrome.tabs.onActivated.addListener(({ tabId }) => { void chrome.tabs.get(tabId).then(paintTab, () => { }); });
let grantView = null;
let iconsConnected = false;
async function paintTab(tab) {
    if (tab.id === undefined)
        return;
    let origin = '';
    try {
        origin = new URL(tab.url ?? '').origin;
    }
    catch { /* not a web page */ }
    const { uses, provides } = roles(grantView, origin);
    await paintIcon(tab.id, iconsConnected, uses, provides).catch(() => { });
}
async function refreshIcons() {
    if (browserConnected) {
        try {
            grantView = await grants('get', '');
        }
        catch {
            grantView = null;
        }
    }
    else
        grantView = null;
    iconsConnected = browserConnected && grantView !== null;
    try {
        await paintIcon(undefined, iconsConnected, false, false);
        for (const tab of await chrome.tabs.query({ active: true }))
            await paintTab(tab);
    }
    catch { /* no window to paint */ }
}
function connectionChanged() { void refreshIcons(); }
chrome.alarms.onAlarm.addListener(alarm => {
    if (alarm.name === 'bridge-reconnect' && !running)
        void run(++generation);
    if (alarm.name === 'bridge-reconnect' && browserConnected)
        void refreshIcons();
});
void chrome.alarms.create('bridge-reconnect', { periodInMinutes: 0.5 });
void run(++generation);
async function groupTabByURL(params) {
    const url = typeof params?.url === 'string' ? params.url : '';
    const title = typeof params?.title === 'string' ? params.title.trim() : '';
    if (!url || !title || !chrome.tabs.group || !chrome.tabGroups)
        return;
    const tabs = await chrome.tabs.query({});
    const match = [...tabs].reverse().find(t => t.id !== undefined && (t.url === url || t.pendingUrl === url));
    if (!match?.id)
        return;
    const groups = await chrome.tabGroups.query({ windowId: match.windowId });
    const existing = groups.find(g => g.title === title);
    if (existing) {
        await chrome.tabs.group({ groupId: existing.id, tabIds: [match.id] });
    }
    else {
        const groupId = await chrome.tabs.group({ tabIds: [match.id] });
        await chrome.tabGroups.update(groupId, { title, color: 'blue' });
    }
}
async function run(currentGeneration) {
    const abort = connectionAbort;
    running = true;
    let failures = 0;
    try {
        while (currentGeneration === generation) {
            const currentBoot = bootId;
            const settings = await bridgeSettings();
            if (!settings.token || settings.token.length < 32)
                return;
            const peer = { instance_id: settings.instanceId, boot_id: currentBoot, label: settings.label || 'Browser profile', browser: navigator.userAgent, extension_version: VERSION };
            if (currentGeneration !== generation)
                return;
            try {
                await commandStream(settings.address, settings.token, peer, abort.signal, () => {
                    if (currentGeneration !== generation)
                        return;
                    browserConnected = true;
                    connectionChanged();
                    failures = 0;
                }, command => {
                    if (currentGeneration !== generation)
                        return;
                    if (command.method === 'tabs.group') {
                        void groupTabByURL(command.params || {}).catch(() => { });
                    }
                });
            }
            catch (error) {
                if (currentGeneration === generation)
                    browserConnected = false;
                failures++;
                console.warn('Local Runtime MCP web bridge disconnected:', error instanceof Error ? error.message : 'connection error');
            }
            finally {
                if (currentGeneration === generation) {
                    browserConnected = false;
                    connectionChanged();
                    tabHandles.clear();
                }
            }
            if (failures >= 2)
                return;
            if (currentGeneration === generation)
                await delay(1000);
        }
    }
    finally {
        if (currentGeneration === generation)
            running = false;
    }
}
function tabHandle(tabId) {
    let handle = tabHandles.get(tabId);
    if (!handle) {
        handle = `${bootId}.${crypto.randomUUID()}`;
        tabHandles.set(tabId, handle);
    }
    return handle;
}
function delay(milliseconds) { return new Promise(resolve => setTimeout(resolve, milliseconds)); }
