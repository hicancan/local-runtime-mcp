function () {
  if (location.origin !== 'https://www.icourse163.org' || !/^\/learn\//.test(location.pathname) || !location.hash.startsWith('#/learn/content')) throw new Error('Open the MOOC courseware outline');
  const links = [...document.querySelectorAll('a[href^="/course/"]')].filter(link => link.textContent.trim());
  const chapters = [...document.querySelectorAll('.m-learnChapterNormal')];
  if (links.length !== 1 || !chapters.length || chapters.length > 100) throw new Error('The course outline is not ready');
  const title = links[0].textContent.trim();
  const url = new URL(links[0].getAttribute('href'), location.origin).href;
  return { course: { title, url }, chapters: chapters.map(chapter => {
    const heading = chapter.querySelector('h3.j-titleName');
    if (!heading) throw new Error('The course outline structure changed');
    const children = [...chapter.querySelectorAll('h4')];
    if (children.length > 1000) throw new Error('The chapter exceeds the observation limit');
    return { title: heading.textContent.trim(), itemsLoaded: children.length > 0, items: children.map(child => {
      const parent = child.parentElement;
      const details = parent.innerText.trim().slice(0, 5000);
      return { title: child.textContent.trim(), kind: parent.classList.contains('quiz') ? 'quiz' : details.includes('视频') ? 'video' : 'unknown', details };
    }) };
  }) };
}
