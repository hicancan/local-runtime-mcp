import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../../capabilities/icourse/outline.js', import.meta.url), 'utf8');
function observe(chapters, override = {}) {
  const context = vm.createContext({ URL, location: { origin: 'https://www.icourse163.org', pathname: '/learn/NJUPT-1462243163', hash: '#/learn/content' }, document: { querySelectorAll(selector) {
    if (selector === 'a[href^="/course/"]') return [{ textContent: '云计算技术', getAttribute: () => '/course/NJUPT-1462243163' }];
    if (selector === '.m-learnChapterNormal') return chapters;
    throw new Error('Unexpected page access');
  } }, ...override });
  return JSON.parse(JSON.stringify(vm.runInContext(`(${source})()`, context)));
}
const chapter = (title, children) => ({ querySelector: () => ({ textContent: title }), querySelectorAll: () => children });
const item = (title, details, quiz = false) => ({ textContent: title, parentElement: { innerText: details, classList: { contains: name => name === 'quiz' && quiz } } });

test('observed outline distinguishes unexpanded content without reading account data or inventing completeness', () => {
  const value = observe([chapter('第1讲', []), chapter('第6讲', [item('6.1 云数据中心能耗', '6.1 云数据中心能耗\n视频 授课视频'), item('第6讲 测验', '提交截止时间：2026/12/24', true)])]);
  assert.deepEqual(value.course, { title: '云计算技术', url: 'https://www.icourse163.org/course/NJUPT-1462243163' });
  assert.equal(value.chapters[0].itemsLoaded, false);
  assert.deepEqual(value.chapters[0].items, []);
  assert.equal(value.chapters[1].itemsLoaded, true);
  assert.deepEqual(value.chapters[1].items.map(x => x.kind), ['video', 'quiz']);
});
test('wrong origin, quiz page and unloaded outline cannot masquerade as a ready course outline', () => {
  assert.throws(() => observe([], { location: { origin: 'https://foreign.example', pathname: '/learn/x', hash: '#/learn/content' } }));
  assert.throws(() => observe([], { location: { origin: 'https://www.icourse163.org', pathname: '/learn/x', hash: '#/learn/test' } }));
  assert.throws(() => observe([]));
});
