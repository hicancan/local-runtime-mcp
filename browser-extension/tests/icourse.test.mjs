import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../../capabilities/icourse/outline.js', import.meta.url), 'utf8');
function observe(chapters, override = {}) {
  const modernChapters = override.modernChapters || [];
  const context = vm.createContext({ URL, location: { origin: 'https://www.icourse163.org', pathname: '/learn/NJUPT-1462243163', hash: '#/learn/content' }, document: { querySelectorAll(selector) {
    if (selector === 'a[href^="/course/"]') return [{ textContent: '云计算技术', getAttribute: () => '/course/NJUPT-1462243163' }];
    if (selector === 'details.course-outline-chapter') return modernChapters;
    if (selector === '.m-learnChapterNormal') return chapters;
    throw new Error('Unexpected page access: ' + selector);
  } }, ...override });
  return JSON.parse(JSON.stringify(vm.runInContext(`(${source})()`, context)));
}
const chapter = (title, children) => ({ querySelector: () => ({ textContent: title }), querySelectorAll: () => children });
const item = (title, details, quiz = false) => ({ textContent: title, parentElement: { innerText: details, classList: { contains: name => name === 'quiz' && quiz } } });

const modernLesson = (title, details, unitTypes = []) => ({
  getAttribute: (k) => (k === 'data-testid' ? 'course-outline-lesson' : null),
  innerText: details,
  querySelector: (sel) => (sel === 'h4' ? { textContent: title } : null),
  querySelectorAll: (sel) => (sel === '[data-unit-type]' ? unitTypes.map((u) => ({ getAttribute: () => u })) : []),
});
const modernAssessment = (details) => ({
  getAttribute: (k) => (k === 'data-testid' ? 'course-outline-assessment-link' : null),
  innerText: details,
  querySelector: () => null,
  querySelectorAll: () => [],
});
const modernChapter = (title, entries) => ({
  querySelector: (sel) => (sel.includes('summary') ? { textContent: title } : null),
  querySelectorAll: () => entries,
});

test('observed outline distinguishes unexpanded content without reading account data or inventing completeness', () => {
  const value = observe([chapter('第1讲', []), chapter('第6讲', [item('6.1 云数据中心能耗', '6.1 云数据中心能耗\n视频 授课视频'), item('第6讲 测验', '提交截止时间：2026/12/24', true)])]);
  assert.deepEqual(value.course, { title: '云计算技术', url: 'https://www.icourse163.org/course/NJUPT-1462243163' });
  assert.equal(value.chapters[0].itemsLoaded, false);
  assert.deepEqual(value.chapters[0].items, []);
  assert.equal(value.chapters[1].itemsLoaded, true);
  assert.deepEqual(value.chapters[1].items.map(x => x.kind), ['video', 'quiz']);
});

test('modern /mooc/learn/<course>/content layout parses expanded lessons, chapter assessments, and collapsed weeks', () => {
  const value = observe([], {
    location: { origin: 'https://www.icourse163.org', pathname: '/mooc/learn/NJUPT-1001639008/content', hash: '' },
    modernChapters: [
      modernChapter('第一周', [
        modernLesson('1.1 计算机网络发展过程', '1.1 计算机网络发展过程\n视频 四个阶段', ['video', 'pdf']),
        modernAssessment('第一周单元测验\n提交截止时间：2026/10/11 08:30\n已完成'),
      ]),
      modernChapter('第二周', []),
    ],
  });
  assert.equal(value.course.title, '云计算技术');
  assert.equal(value.chapters.length, 2);
  assert.equal(value.chapters[0].itemsLoaded, true);
  assert.deepEqual(value.chapters[0].items.map(x => [x.title, x.kind]), [
    ['1.1 计算机网络发展过程', 'video'],
    ['第一周单元测验', 'quiz'],
  ]);
  assert.equal(value.chapters[1].itemsLoaded, false);
  assert.deepEqual(value.chapters[1].items, []);
});

test('wrong origin, quiz page, player page and unloaded outline cannot masquerade as a ready course outline', () => {
  assert.throws(() => observe([], { location: { origin: 'https://foreign.example', pathname: '/learn/x', hash: '#/learn/content' } }));
  assert.throws(() => observe([], { location: { origin: 'https://www.icourse163.org', pathname: '/learn/x', hash: '#/learn/test' } }));
  assert.throws(() => observe([], { location: { origin: 'https://www.icourse163.org', pathname: '/mooc/learn/x/content/1278945696', hash: '' } }));
  assert.throws(() => observe([]));
});
