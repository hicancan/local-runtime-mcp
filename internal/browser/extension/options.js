import { packagedConfig } from './runtime_config.js';
const address = document.querySelector('#address');
const token = document.querySelector('#token');
const label = document.querySelector('#label');
const status = document.querySelector('#status');
const identity = document.querySelector('#identity');
const stored = await chrome.storage.local.get({ address: packagedConfig.address || '127.0.0.1:9315', token: packagedConfig.token || '', label: '', instanceId: '' });
address.value = stored.address;
token.value = stored.token;
label.value = stored.label;
identity.textContent = stored.instanceId ? `Browser ID: ${stored.instanceId}` : 'Browser ID is assigned on first connection.';
document.querySelector('#save').addEventListener('click', async () => {
    const nextAddress = address.value.trim(), nextToken = token.value.trim();
    if (!/^(127\.0\.0\.1|localhost|\[::1\]):\d+$/.test(nextAddress)) {
        status.textContent = 'Use a loopback host and port.';
        return;
    }
    if (nextToken.length < 32) {
        status.textContent = 'Token must have at least 32 characters.';
        return;
    }
    await chrome.storage.local.set({ address: nextAddress, token: nextToken, label: label.value.trim().slice(0, 120) });
    await chrome.runtime.sendMessage({ type: 'settings-changed' });
    status.textContent = 'Saved. Connecting…';
});
