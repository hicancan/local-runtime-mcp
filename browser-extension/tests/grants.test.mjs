import test from 'node:test';
import assert from 'node:assert/strict';
import { covers, roles } from '../../internal/browser/extension/grants.js';

test('a site grant covers its subdomains, while a local origin covers only itself', () => {
  assert.equal(covers('https://chaoxing.com', 'https://mooc2-ans.chaoxing.com'), true);
  assert.equal(covers('https://chaoxing.com', 'https://chaoxing.com'), true);
  assert.equal(covers('https://chaoxing.com', 'https://evilchaoxing.com'), false);
  assert.equal(covers('https://chaoxing.com', 'http://mooc1.chaoxing.com'), false);
  assert.equal(covers('http://127.0.0.1:4321', 'http://127.0.0.1:4324'), false);
  assert.equal(covers('http://127.0.0.1:4321', 'http://127.0.0.1:4321'), true);
});

test('the toolbar roles follow real grants only', () => {
  const view = { site: '', pageTools: [], machine: [], proposals: [], grants: [
    { Site: 'https://hicancan.top', Use: true, Tools: [] },
    { Site: 'https://icourse163.org', Provide: true },
  ] };
  assert.deepEqual(roles(view, 'https://studev.hicancan.top'), { uses: true, provides: false });
  assert.deepEqual(roles(view, 'https://www.icourse163.org'), { uses: false, provides: true });
  assert.deepEqual(roles(null, 'https://studev.hicancan.top'), { uses: false, provides: false });
});
