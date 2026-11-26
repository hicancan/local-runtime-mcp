function () {
  const isLegacyRoute = /^\/learn\//.test(location.pathname) && location.hash.startsWith('#/learn/content');
  const isModernRoute = /^\/mooc\/learn\/[^/]+\/content\/?$/.test(location.pathname);
  if (location.origin !== 'https://www.icourse163.org' || (!isLegacyRoute && !isModernRoute)) {
    throw new Error('Open the MOOC courseware outline');
  }
  const links = [...document.querySelectorAll('a[href^="/course/"]')].filter(link => link.textContent.trim());
  if (links.length !== 1) throw new Error('The course outline is not ready');
  const title = links[0].textContent.trim();
  const url = new URL(links[0].getAttribute('href'), location.origin).href;

  // Modern MOOC courseware layout (/mooc/learn/<course>/content):
  // <details class="course-outline-chapter"> containing lessons + assessment links.
  const modernChapters = [...document.querySelectorAll('details.course-outline-chapter')];
  if (modernChapters.length > 0) {
    if (modernChapters.length > 100) throw new Error('The course outline is not ready');
    return {
      course: { title, url },
      chapters: modernChapters.map(chapter => {
        const heading = chapter.querySelector('summary .course-outline-chapter-title-text') || chapter.querySelector('summary');
        if (!heading || !heading.textContent.trim()) throw new Error('The course outline structure changed');
        const entries = [...chapter.querySelectorAll('[data-testid="course-outline-lesson"], [data-testid="course-outline-assessment-link"]')];
        if (entries.length > 1000) throw new Error('The chapter exceeds the observation limit');
        return {
          title: heading.textContent.trim(),
          itemsLoaded: entries.length > 0,
          items: entries.map(entry => {
            const testId = entry.getAttribute('data-testid');
            const details = (entry.innerText || entry.textContent || '').trim().slice(0, 5000);
            if (testId === 'course-outline-assessment-link') {
              const titleEl = entry.querySelector('.course-outline-assessment-main .truncate') || entry.querySelector('h4');
              const detailEl = entry.querySelector('.course-outline-assessment-detail');
              const resultEl = entry.querySelector('.course-outline-assessment-result');
              const assessTitle = (titleEl ? titleEl.textContent : details.split(/\r?\n/)[0]).trim();
              const structuredDetails = [assessTitle, detailEl && detailEl.textContent.trim(), resultEl && resultEl.textContent.trim()].filter(Boolean).join('\n');
              return { title: assessTitle || '测验', kind: 'quiz', details: (structuredDetails || details).slice(0, 5000) };
            }
            const lessonHeading = entry.querySelector('h4');
            const lessonTitle = (lessonHeading ? lessonHeading.textContent : details.split(/\r?\n/)[0]).trim();
            const unitTypes = [...entry.querySelectorAll('[data-unit-type]')].map(n => n.getAttribute('data-unit-type'));
            const kind = unitTypes.includes('video') ? 'video'
              : (unitTypes.includes('quiz') ? 'quiz'
                : (details.includes('视频') ? 'video' : (details.includes('测验') ? 'quiz' : 'unknown')));
            return { title: lessonTitle, kind, details };
          }),
        };
      }),
    };
  }

  // Legacy MOOC courseware layout (/learn/<course>#/learn/content):
  const chapters = [...document.querySelectorAll('.m-learnChapterNormal')];
  if (!chapters.length || chapters.length > 100) throw new Error('The course outline is not ready');
  return {
    course: { title, url },
    chapters: chapters.map(chapter => {
      const heading = chapter.querySelector('h3.j-titleName');
      if (!heading) throw new Error('The course outline structure changed');
      const children = [...chapter.querySelectorAll('h4')];
      if (children.length > 1000) throw new Error('The chapter exceeds the observation limit');
      return {
        title: heading.textContent.trim(),
        itemsLoaded: children.length > 0,
        items: children.map(child => {
          const parent = child.parentElement;
          const details = parent.innerText.trim().slice(0, 5000);
          return {
            title: child.textContent.trim(),
            kind: parent.classList.contains('quiz') ? 'quiz' : details.includes('视频') ? 'video' : 'unknown',
            details,
          };
        }),
      };
    }),
  };
}
