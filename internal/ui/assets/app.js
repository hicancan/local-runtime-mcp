(() => {
  const I18N = {
    zh: {
      navAll: '实时全景',
      navProposals: '提案审批',
      navWires: '网线与授权',
      searchPlaceholder: '搜索命令、网页、笔记或 Agent…',
      refresh: '刷新',
      exportJSONL: '导出 JSONL',
      kickSession: '断开会话',
      stopDesktop: '紧急收回键鼠',
      grpCloud: '云端接入',
      grpLocal: '本机客户端',
      grpWeb: '网页与系统',
      emptyTimelineTitle: '等待 Agent 接入活动流',
      emptyTimelineSub: '任何本地或云端 Agent 调用工具或发送 agent_report 后，时间线会在这里毫秒级实时出现。',
      emptySearchTitle: '没有匹配记录',
      emptySearchSub: '换一个关键词试试（支持中英文全文检索）。',
      emptyProposalsTitle: '暂无待审批网页能力提案',
      emptyProposalsSub: '当 Agent 通过 capability_propose 提炼出新的站点能力时，会在这里等待你审查批准。',
      ranOps: (n) => `运行了 ${n} 项操作`,
      argsLabel: '参数 (已脱敏)',
      resultLabel: '结果',
      errorLabel: '错误',
      approve: '批准上线',
      reject: '拒绝',
      readOnly: '只读',
      mutating: '可写入',
      remoteReadOnly: '仅允许只读工具',
      wireAttach: '接入',
      wireDetach: '断开',
      noTTL: '无自动断开',
      tokensTitle: '云端多 Agent 独立 Token (9316)',
      tokensEmpty: '尚未配置 grants.remote.tokens；运行 lrmcp setup remote-token --id astra-1 创建。',
      browsersTitle: '已连接浏览器实例',
      browsersEmpty: '暂无在线浏览器扩展。',
      webGrantsTitle: '已授权站点',
      webGrantsEmpty: '暂无站点授权，可在浏览器右上角 L 图标面板随时开启。',
      kickConfirm: '确认断开该会话并标记为已关闭？',
      toastApproved: '已批准上线',
      toastRejected: '已拒绝提案',
      toastDesktopStopped: '已紧急收回键鼠控制权',
      toastKicked: '会话已断开',
      searchTitle: (q) => `全文检索：“${q}”`,
      justNow: '刚刚',
    },
    en: {
      navAll: 'Live Feed',
      navProposals: 'Proposals',
      navWires: 'Wires & Grants',
      searchPlaceholder: 'Search tools, pages, notes, agents…',
      refresh: 'Refresh',
      exportJSONL: 'Export JSONL',
      kickSession: 'Kick Session',
      stopDesktop: 'Stop Desktop',
      grpCloud: 'Cloud Wires',
      grpLocal: 'Local Clients',
      grpWeb: 'Web & System',
      emptyTimelineTitle: 'Waiting for live runtime activity',
      emptyTimelineSub: 'Tool calls and agent_report progress notes from local or cloud clients appear here in real time.',
      emptySearchTitle: 'No matching events',
      emptySearchSub: 'Try another keyword (FTS5 full-text search across tools, args, outputs, and notes).',
      emptyProposalsTitle: 'No pending Web MCP proposals',
      emptyProposalsSub: 'Drafts created via capability_propose appear here for your review before anything runs.',
      ranOps: (n) => `Ran ${n} ${n === 1 ? 'operation' : 'operations'}`,
      argsLabel: 'Arguments (Redacted)',
      resultLabel: 'Result',
      errorLabel: 'Error',
      approve: 'Approve',
      reject: 'Reject',
      readOnly: 'Read-only',
      mutating: 'Mutating',
      remoteReadOnly: 'Read-only tools only',
      wireAttach: 'Attach',
      wireDetach: 'Detach',
      noTTL: 'No TTL',
      tokensTitle: 'Named Remote Tokens (9316)',
      tokensEmpty: 'No grants.remote.tokens configured. Run `lrmcp setup remote-token --id astra-1` to mint one.',
      browsersTitle: 'Connected Browser Profiles',
      browsersEmpty: 'No Chromium extension connected.',
      webGrantsTitle: 'Website Grants',
      webGrantsEmpty: 'No site grants configured yet; toggle them from the L extension popup.',
      kickConfirm: 'Disconnect this session and mark it closed?',
      toastApproved: 'Proposal approved',
      toastRejected: 'Proposal rejected',
      toastDesktopStopped: 'Desktop physical control revoked',
      toastKicked: 'Session closed',
      searchTitle: (q) => `Search: "${q}"`,
      justNow: 'now',
    },
  };

  let lang = localStorage.getItem('lrmcp_ui_lang') || (navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en');
  let theme = localStorage.getItem('lrmcp_ui_theme') || (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');

  const state = {
    view: 'all', // 'all' | 'session' | 'proposals' | 'wires' | 'search'
    sessionId: null,
    searchQuery: '',
    status: null,
    sessions: [],
    events: [],
    proposals: [],
    openRows: new Set(),
    expandedGroups: new Set(),
  };

  const $ = (id) => document.getElementById(id);
  const t = (k, ...args) => {
    const val = (I18N[lang] || I18N.en)[k];
    return typeof val === 'function' ? val(...args) : (val || k);
  };

  function applyThemeAndLang() {
    document.documentElement.dataset.theme = theme;
    document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
    $('langBtn').textContent = lang === 'zh' ? 'EN' : '中文';
    $('searchInput').placeholder = t('searchPlaceholder');
    for (const node of document.querySelectorAll('[data-i18n]')) {
      node.textContent = t(node.dataset.i18n);
    }
  }

  function showToast(msg) {
    const el = $('toast');
    el.textContent = msg;
    el.classList.remove('hidden');
    clearTimeout(showToast._t);
    showToast._t = setTimeout(() => el.classList.add('hidden'), 2400);
  }

  async function apiGet(path) {
    const res = await fetch(path, { credentials: 'same-origin' });
    if (res.status === 401) { location.reload(); throw new Error('unauthorized'); }
    if (!res.ok) throw new Error(await res.text());
    return res.json();
  }

  async function apiPost(path, body) {
    const res = await fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-LRMCP-UI': '1' },
      body: JSON.stringify(body || {}),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data?.error?.message || `HTTP ${res.status}`);
    return data;
  }

  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') node.className = v;
      else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2).toLowerCase(), v);
      else if (v !== null && v !== undefined && v !== false) node.setAttribute(k, v === true ? '' : String(v));
    }
    for (const c of children) {
      if (c === null || c === undefined || c === false) continue;
      node.append(typeof c === 'string' ? document.createTextNode(c) : c);
    }
    return node;
  }

  function relTime(ms) {
    if (!ms) return '';
    const diff = Math.max(0, Math.floor((Date.now() - ms) / 1000));
    if (diff < 20) return t('justNow');
    if (diff < 60) return `${diff}s`;
    if (diff < 3600) return `${Math.floor(diff / 60)}m`;
    if (diff < 86400) return `${Math.floor(diff / 3600)}h`;
    return `${Math.floor(diff / 86400)}d`;
  }

  function formatClock(ms) {
    if (!ms) return '';
    const d = new Date(ms);
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  }

  function formatDur(ms) {
    if (!ms || ms <= 0) return '';
    if (ms < 1000) return `${ms}ms`;
    return `${(ms / 1000).toFixed(1)}s`;
  }

  function prettyJSON(raw) {
    if (!raw || raw === '{}' || raw === 'null') return '';
    try { return JSON.stringify(JSON.parse(raw), null, 2); } catch { return raw; }
  }

  function wireBucket(wire) {
    if (wire === 'cloudflare' || wire === 'openai' || wire === 'http') return 'cloud';
    if (wire === 'local' || wire === 'local-client') return 'local';
    return 'web';
  }

  function renderSidebar() {
    for (const btn of document.querySelectorAll('.nav-item')) {
      btn.classList.toggle('active', state.view === btn.dataset.view);
    }
    const stats = state.status?.audit_stats;
    $('totalEventsBadge').textContent = String(stats?.events ?? state.events.length);
    const pCount = state.proposals.length;
    $('proposalsBadge').textContent = String(pCount);
    $('proposalsBadge').classList.toggle('hidden', pCount === 0);
    const activeWires = (state.status?.tunnels || []).filter((w) => w.state === 'active').length;
    $('wiresBadge').textContent = String(activeWires);

    const groups = { cloud: [], local: [], web: [] };
    for (const s of state.sessions) {
      groups[wireBucket(s.wire)].push(s);
    }

    const host = $('sessionGroups');
    host.replaceChildren();
    const defs = [
      ['cloud', t('grpCloud')],
      ['local', t('grpLocal')],
      ['web', t('grpWeb')],
    ];
    for (const [key, label] of defs) {
      const list = groups[key];
      if (!list.length) continue;
      const wrap = el('div', {}, el('div', { class: 'group-label' }, label));
      for (const s of list) {
        const isActive = state.view === 'session' && state.sessionId === s.id;
        const dotClass = s.status === 'working' ? 'working' : (s.status === 'error' ? 'error' : (Date.now() - s.last_seen_at < 300000 && s.status !== 'closed' ? 'active' : ''));
        wrap.append(el('button', {
          type: 'button',
          class: `session-item${isActive ? ' active' : ''}`,
          onClick: () => selectSession(s.id),
        },
          el('span', { class: `s-dot ${dotClass}` }),
          el('div', { class: 's-body' },
            el('div', { class: 's-top' },
              el('span', { class: 's-title' }, s.title || s.agent_id || s.id),
              el('span', { class: 's-time' }, relTime(s.last_seen_at)),
            ),
            el('div', { class: 's-sub' },
              el('span', { class: 'wire-tag' }, s.wire),
              el('span', {}, `${s.tool_count} ops`),
              s.error_count > 0 ? el('span', { style: 'color:var(--err)' }, `${s.error_count} err`) : null,
            ),
          ),
        ));
      }
      host.append(wrap);
    }

    if (state.status) {
      $('versionLabel').textContent = `v${state.status.version}`;
      const kb = stats?.db_bytes ? `${Math.max(1, Math.round(stats.db_bytes / 1024))} KB` : 'WAL';
      $('dbStatsLabel').textContent = `${stats?.sessions ?? 0}s · ${kb}`;
    }
  }

  function renderSovereignBar() {
    const st = state.status;
    if (!st) return;
    const pills = $('wireControls');
    pills.replaceChildren();
    for (const w of (st.tunnels || [])) {
      if (!w.configured && w.state !== 'active' && w.state !== 'error') continue;
      const cls = w.state === 'active' ? 'wire-btn on' : (w.state === 'error' ? 'wire-btn err' : 'wire-btn');
      const ttlTxt = w.ttl_remaining_seconds ? ` · ${Math.ceil(w.ttl_remaining_seconds / 60)}m` : '';
      pills.append(el('button', {
        type: 'button',
        class: cls,
        title: w.last_error || w.address || w.name,
        onClick: () => toggleWire(w.name, w.state !== 'active', 0),
      }, el('span', { class: 'dot-sm' }), `${w.name}${ttlTxt}`));
    }

    const browsers = (st.browser?.instances || []).filter((b) => b.connected).length;
    $('browserPill').textContent = `Browser: ${browsers}`;
    $('stdioPill').textContent = `Local: ${st.attached_stdio ?? 0}`;
    const owned = st.desktop_control?.status === 'owned';
    $('stopDesktopBtn').classList.toggle('hidden', !owned);
  }

  function renderTopbar() {
    const meta = $('viewMeta');
    meta.replaceChildren();
    const kickBtn = $('kickBtn');
    const exportBtn = $('exportBtn');
    kickBtn.classList.add('hidden');

    if (state.view === 'all') {
      $('viewTitle').textContent = t('navAll');
      exportBtn.href = '/ui/api/export';
      exportBtn.classList.remove('hidden');
    } else if (state.view === 'search') {
      $('viewTitle').textContent = t('searchTitle', state.searchQuery);
      exportBtn.classList.add('hidden');
    } else if (state.view === 'proposals') {
      $('viewTitle').textContent = t('navProposals');
      exportBtn.classList.add('hidden');
    } else if (state.view === 'wires') {
      $('viewTitle').textContent = t('navWires');
      exportBtn.classList.add('hidden');
    } else if (state.view === 'session') {
      const s = state.sessions.find((x) => x.id === state.sessionId);
      $('viewTitle').textContent = s ? (s.title || s.agent_id || s.id) : state.sessionId;
      if (s) {
        meta.append(el('span', { class: `pill ${wireBucket(s.wire) === 'cloud' ? 'cloud' : ''}` }, s.wire));
        if (s.agent_id && s.agent_id !== s.title) meta.append(el('span', { class: 'pill' }, s.agent_id));
        if (s.status) meta.append(el('span', { class: `pill ${s.status === 'error' ? 'err' : (s.status === 'working' ? 'ok' : '')}` }, s.status));
        if (s.status !== 'closed' && s.wire !== 'system') kickBtn.classList.remove('hidden');
      }
      exportBtn.href = `/ui/api/export?session_id=${encodeURIComponent(state.sessionId || '')}`;
      exportBtn.classList.remove('hidden');
    }
  }

  function buildBlocks(events) {
    const blocks = [];
    let currentTools = null;
    const flush = () => {
      if (currentTools) { blocks.push(currentTools); currentTools = null; }
    };
    for (const ev of events) {
      if (ev.kind === 'note' || ev.kind === 'system') {
        flush();
        blocks.push({ type: ev.kind, event: ev });
      } else {
        if (!currentTools || (state.view === 'all' && currentTools.sessionId !== ev.session_id)) {
          flush();
          currentTools = { type: 'tools', sessionId: ev.session_id, agentId: ev.agent_id, wire: ev.wire, events: [] };
        }
        currentTools.events.push(ev);
      }
    }
    flush();
    return blocks;
  }

  function renderToolRow(ev) {
    const isOpen = state.openRows.has(ev.id);
    const isErr = ev.status === 'error';
    const isRun = ev.status === 'running';
    const icon = el('span', { class: `t-icon ${isRun ? 'run' : (isErr ? 'err' : 'ok')}` }, isRun ? '' : (isErr ? '✕' : '✓'));

    const row = el('div', { class: `tool-row${isOpen ? ' open' : ''}${isErr ? ' err' : ''}` },
      el('div', {
        class: 'tool-head',
        onClick: () => {
          if (state.openRows.has(ev.id)) state.openRows.delete(ev.id);
          else state.openRows.add(ev.id);
          renderContent();
        },
      },
        icon,
        el('span', { class: 't-summary' }, ev.summary || ev.tool_name),
        el('div', { class: 't-meta' },
          el('span', { class: 't-tool' }, ev.tool_name),
          ev.duration_ms ? el('span', {}, formatDur(ev.duration_ms)) : null,
          el('span', {}, formatClock(ev.created_at)),
        ),
      ),
    );

    if (isOpen || ev.image_blob) {
      const detail = el('div', { class: 'tool-detail' });
      if (ev.image_blob) {
        const imgUrl = `/ui/api/blobs/${ev.image_blob}`;
        detail.append(el('div', { class: 'thumb-wrap' },
          el('img', { class: 'thumb-img', src: imgUrl, alt: ev.tool_name, onClick: () => openLightbox(imgUrl) }),
        ));
      }
      if (isOpen) {
        const argsStr = prettyJSON(ev.args_json);
        if (argsStr) detail.append(el('div', {}, el('div', { class: 'kv-label' }, t('argsLabel')), el('pre', { class: 'code-box' }, argsStr)));
        if (ev.error_text) detail.append(el('div', {}, el('div', { class: 'kv-label' }, t('errorLabel')), el('pre', { class: 'code-box err-box' }, ev.error_text)));
        const resStr = prettyJSON(ev.result_json);
        if (resStr) detail.append(el('div', {}, el('div', { class: 'kv-label' }, t('resultLabel')), el('pre', { class: 'code-box' }, resStr)));
      }
      row.append(detail);
    }
    return row;
  }

  function renderTimeline(events, isSearch) {
    const col = el('div', { class: 'stream-col' });
    if (!events.length) {
      col.append(el('div', { class: 'empty-state' },
        el('h2', {}, isSearch ? t('emptySearchTitle') : t('emptyTimelineTitle')),
        el('p', {}, isSearch ? t('emptySearchSub') : t('emptyTimelineSub')),
      ));
      return col;
    }

    const blocks = buildBlocks(events);
    const lastIdx = blocks.length - 1;
    blocks.forEach((blk, idx) => {
      if (blk.type === 'system') {
        const ev = blk.event;
        col.append(el('div', { class: 'sys-row' }, `${ev.summary} · ${formatClock(ev.created_at)}`));
        return;
      }
      if (blk.type === 'note') {
        const ev = blk.event;
        col.append(el('article', { class: 'note-block' },
          el('div', { class: 'note-head' },
            el('span', { class: 'note-agent' }, ev.agent_id),
            el('span', { class: 'pill' }, ev.wire),
            el('span', {}, formatClock(ev.created_at)),
          ),
          el('div', { class: 'note-text' }, ev.summary),
        ));
        return;
      }

      const groupKey = `${blk.events[0].id}`;
      const hasLiveOrErrOrImg = blk.events.some((e) => e.status === 'running' || e.status === 'error' || e.image_blob);
      const defaultOpen = isSearch || idx === lastIdx || hasLiveOrErrOrImg || blk.events.length <= 2;
      const isExpanded = state.expandedGroups.has(groupKey) ? !defaultOpen : defaultOpen;
      const totalMS = blk.events.reduce((acc, e) => acc + (e.duration_ms || 0), 0);
      const errCnt = blk.events.filter((e) => e.status === 'error').length;

      const labelParts = [t('ranOps', blk.events.length)];
      if (state.view === 'all' || isSearch) labelParts.push(`· ${blk.agentId} (${blk.wire})`);
      if (totalMS > 0) labelParts.push(`· ${formatDur(totalMS)}`);
      if (errCnt > 0) labelParts.push(`· ${errCnt} err`);

      const grp = el('section', { class: `tool-group${isExpanded ? '' : ' collapsed'}` },
        el('button', {
          type: 'button',
          class: 'group-toggle',
          onClick: () => {
            if (state.expandedGroups.has(groupKey)) state.expandedGroups.delete(groupKey);
            else state.expandedGroups.add(groupKey);
            renderContent();
          },
        }, el('span', {}, labelParts.join(' ')), el('span', { class: 'chev' }, '▾')),
        el('div', { class: 'tool-rail' }, ...blk.events.map(renderToolRow)),
      );
      col.append(grp);
    });
    return col;
  }

  function renderProposals() {
    const col = el('div', { class: 'stream-col' });
    if (!state.proposals.length) {
      col.append(el('div', { class: 'empty-state' },
        el('h2', {}, t('emptyProposalsTitle')),
        el('p', {}, t('emptyProposalsSub')),
      ));
      return col;
    }
    for (const p of state.proposals) {
      const d = p.definition || {};
      const ro = Boolean(d.annotations?.readOnlyHint);
      col.append(el('div', { class: 'card' },
        el('div', { class: 'card-head' },
          el('div', { class: 'card-title' }, `${d.title || d.name} (${d.name})`),
          el('span', { class: `pill ${ro ? 'ok' : 'err'}` }, ro ? t('readOnly') : t('mutating')),
        ),
        el('p', { class: 'card-desc' }, d.description || ''),
        el('div', { class: 'card-row' }, el('span', {}, (d.origins || []).join(', '))),
        el('pre', { class: 'code-box' }, p.script || ''),
        el('div', { style: 'display:flex;gap:8px;justify-content:flex-end' },
          el('button', { type: 'button', class: 'btn danger-ghost', onClick: () => decideProposal('reject', d.name) }, t('reject')),
          el('button', { type: 'button', class: 'btn primary', onClick: () => decideProposal('approve', d.name) }, t('approve')),
        ),
      ));
    }
    return col;
  }

  function renderWires() {
    const col = el('div', { class: 'stream-col' });
    const st = state.status;
    if (!st) return col;

    const grid = el('div', { class: 'card-grid' });
    const rg = st.remote_grants || { openai: {}, http: {}, tokens: [] };

    for (const w of (st.tunnels || [])) {
      const active = w.state === 'active';
      const target = w.name === 'openai' ? 'openai' : 'http';
      const grant = target === 'openai' ? (rg.openai || {}) : (rg.http || {});
      const ttlSel = el('select', { class: 'select-sm' },
        el('option', { value: '0' }, t('noTTL')),
        el('option', { value: '1800' }, '30m'),
        el('option', { value: '7200' }, '2h'),
        el('option', { value: '28800' }, '8h'),
      );
      const roBox = el('input', { type: 'checkbox', checked: Boolean(grant.read_only) });
      roBox.addEventListener('change', () => updateRemoteGrant(target, { ...grant, read_only: roBox.checked }));

      grid.append(el('div', { class: 'card' },
        el('div', { class: 'card-head' },
          el('span', { class: 'card-title' }, w.name.toUpperCase()),
          el('span', { class: `pill ${active ? 'ok' : (w.state === 'error' ? 'err' : '')}` }, w.state),
        ),
        w.address ? el('div', { class: 'card-row' }, el('span', { style: 'font-family:var(--mono)' }, w.address)) : null,
        w.last_error ? el('div', { class: 'code-box err-box' }, w.last_error) : null,
        el('label', { class: 'check-row' }, roBox, t('remoteReadOnly')),
        el('div', { class: 'card-row' },
          ttlSel,
          el('button', {
            type: 'button',
            class: active ? 'btn danger-ghost' : 'btn primary',
            disabled: !w.configured && !active,
            onClick: () => toggleWire(w.name, !active, Number(ttlSel.value)),
          }, active ? t('wireDetach') : t('wireAttach')),
        ),
      ));
    }
    col.append(grid);

    // Named remote tokens table
    const tokens = rg.tokens || [];
    const tokCard = el('div', { class: 'card' }, el('div', { class: 'card-title' }, t('tokensTitle')));
    if (!tokens.length) {
      tokCard.append(el('p', { class: 'card-desc' }, t('tokensEmpty')));
    } else {
      const tbl = el('table', { class: 'mini' },
        el('thead', {}, el('tr', {}, el('th', {}, 'ID'), el('th', {}, 'Mode'), el('th', {}, 'Tools'))),
        el('tbody', {}, ...tokens.map((tk) => el('tr', {},
          el('td', { style: 'font-family:var(--mono)' }, tk.id),
          el('td', {}, tk.read_only ? t('readOnly') : 'Full'),
          el('td', { style: 'font-family:var(--mono)' }, (tk.tools || []).join(', ') || '*'),
        ))),
      );
      tokCard.append(el('div', { class: 'table-wrap' }, tbl));
    }
    col.append(tokCard);

    // Connected browsers
    const bList = st.browser?.instances || [];
    const bCard = el('div', { class: 'card' }, el('div', { class: 'card-title' }, t('browsersTitle')));
    if (!bList.length) {
      bCard.append(el('p', { class: 'card-desc' }, t('browsersEmpty')));
    } else {
      for (const b of bList) {
        bCard.append(el('div', { class: 'card-row' },
          el('span', { style: 'font-family:var(--mono)' }, b.browser_id),
          el('span', { class: `pill ${b.connected ? 'ok' : ''}` }, b.connected ? 'connected' : 'offline'),
        ));
      }
    }
    col.append(bCard);

    // Web grants summary
    const wg = st.web_grants || [];
    const wgCard = el('div', { class: 'card' }, el('div', { class: 'card-title' }, t('webGrantsTitle')));
    if (!wg.length) {
      wgCard.append(el('p', { class: 'card-desc' }, t('webGrantsEmpty')));
    } else {
      for (const g of wg) {
        wgCard.append(el('div', { class: 'card-row' },
          el('span', { style: 'font-family:var(--mono)' }, g.Site),
          el('div', { style: 'display:flex;gap:6px' },
            g.Use ? el('span', { class: 'pill ok' }, 'Use') : null,
            g.Provide ? el('span', { class: 'pill cloud' }, 'Provide') : null,
          ),
        ));
      }
    }
    col.append(wgCard);

    return col;
  }

  function renderContent() {
    renderTopbar();
    const area = $('contentArea');
    area.replaceChildren();
    if (state.view === 'proposals') area.append(renderProposals());
    else if (state.view === 'wires') area.append(renderWires());
    else area.append(renderTimeline(state.events, state.view === 'search'));
  }

  function openLightbox(src) {
    $('lightboxImg').src = src;
    $('lightbox').classList.remove('hidden');
  }

  async function loadAll() {
    const [st, sess, props] = await Promise.all([
      apiGet('/ui/api/status'),
      apiGet('/ui/api/sessions?limit=150'),
      apiGet('/ui/api/proposals'),
    ]);
    state.status = st;
    state.sessions = sess.sessions || [];
    state.proposals = props.proposals || [];
    await loadCurrentViewEvents();
    renderSidebar();
    renderSovereignBar();
    renderContent();
  }

  async function loadCurrentViewEvents() {
    if (state.view === 'all') {
      const data = await apiGet('/ui/api/events?limit=200');
      state.events = data.events || [];
    } else if (state.view === 'session' && state.sessionId) {
      const data = await apiGet(`/ui/api/events?session_id=${encodeURIComponent(state.sessionId)}&limit=400`);
      state.events = data.events || [];
    } else if (state.view === 'search' && state.searchQuery) {
      const data = await apiGet(`/ui/api/search?q=${encodeURIComponent(state.searchQuery)}&limit=100`);
      state.events = data.events || [];
    }
  }

  async function selectView(v) {
    state.view = v;
    if (v !== 'session') state.sessionId = null;
    if (v !== 'search') { state.searchQuery = ''; $('searchInput').value = ''; }
    await loadCurrentViewEvents();
    renderSidebar();
    renderContent();
  }

  async function selectSession(id) {
    state.view = 'session';
    state.sessionId = id;
    state.searchQuery = '';
    $('searchInput').value = '';
    await loadCurrentViewEvents();
    renderSidebar();
    renderContent();
  }

  async function toggleWire(name, up, ttlSeconds) {
    try {
      const path = up ? '/ui/api/tunnels/up' : '/ui/api/tunnels/down';
      const body = up ? { name, ttl_seconds: ttlSeconds || 0 } : { name };
      const res = await apiPost(path, body);
      if (state.status) state.status.tunnels = res.tunnels;
      renderSidebar();
      renderSovereignBar();
      if (state.view === 'wires') renderContent();
      showToast(`${name}: ${up ? 'active' : 'stopped'}`);
    } catch (err) {
      showToast(err.message);
    }
  }

  async function updateRemoteGrant(target, grant) {
    try {
      const res = await apiPost('/ui/api/remote-grants', { target, grant });
      if (state.status) state.status.remote_grants = res.remote_grants;
      renderContent();
      showToast('Saved');
    } catch (err) {
      showToast(err.message);
    }
  }

  async function decideProposal(action, name) {
    try {
      const res = await apiPost(`/ui/api/proposals/${action}`, { name });
      state.proposals = res.proposals || [];
      renderSidebar();
      renderContent();
      showToast(action === 'approve' ? t('toastApproved') : t('toastRejected'));
    } catch (err) {
      showToast(err.message);
    }
  }

  function upsertSession(sess) {
    if (!sess) return;
    const idx = state.sessions.findIndex((s) => s.id === sess.id);
    if (idx >= 0) state.sessions[idx] = sess;
    else state.sessions.unshift(sess);
    state.sessions.sort((a, b) => (b.last_seen_at || 0) - (a.last_seen_at || 0));
  }

  function upsertEvent(ev) {
    if (!ev) return;
    if (state.view === 'session' && ev.session_id !== state.sessionId) return;
    if (state.view !== 'all' && state.view !== 'session') return;
    const idx = state.events.findIndex((e) => e.id === ev.id);
    if (idx >= 0) state.events[idx] = ev;
    else state.events.push(ev);
    if (state.events.length > 500) state.events.shift();
  }

  function connectSSE() {
    const es = new EventSource('/ui/api/stream');
    const dot = $('sseDot');
    es.onopen = () => { dot.textContent = 'LIVE'; dot.classList.remove('off'); };
    es.onerror = () => { dot.textContent = 'RECONNECT'; dot.classList.add('off'); };
    const handle = (e) => {
      try {
        const pkt = JSON.parse(e.data);
        if (pkt.session) upsertSession(pkt.session);
        if (pkt.event) {
          upsertEvent(pkt.event);
          if (pkt.event.kind === 'proposal') {
            apiGet('/ui/api/proposals').then((d) => { state.proposals = d.proposals || []; renderSidebar(); }).catch(() => {});
          }
        }
        renderSidebar();
        if (state.view === 'all' || state.view === 'session') {
          const area = $('contentArea');
          const nearBottom = area.scrollHeight - area.scrollTop - area.clientHeight < 90;
          renderContent();
          if (nearBottom) area.scrollTop = area.scrollHeight;
        }
      } catch { /* ignore */ }
    };
    es.addEventListener('event', handle);
    es.addEventListener('session', handle);
  }

  // Wire UI bindings
  $('langBtn').addEventListener('click', () => {
    lang = lang === 'zh' ? 'en' : 'zh';
    localStorage.setItem('lrmcp_ui_lang', lang);
    applyThemeAndLang();
    renderSidebar();
    renderContent();
  });
  $('themeBtn').addEventListener('click', () => {
    theme = theme === 'dark' ? 'light' : 'dark';
    localStorage.setItem('lrmcp_ui_theme', theme);
    applyThemeAndLang();
  });
  $('refreshBtn').addEventListener('click', () => void loadAll());
  $('stopDesktopBtn').addEventListener('click', async () => {
    await apiPost('/ui/api/desktop/stop', {});
    showToast(t('toastDesktopStopped'));
    await loadAll();
  });
  $('kickBtn').addEventListener('click', async () => {
    if (!state.sessionId || !confirm(t('kickConfirm'))) return;
    await apiPost('/ui/api/sessions/kick', { session_id: state.sessionId });
    showToast(t('toastKicked'));
    await loadAll();
  });
  $('lightbox').addEventListener('click', () => $('lightbox').classList.add('hidden'));

  for (const btn of document.querySelectorAll('.nav-item')) {
    btn.addEventListener('click', () => void selectView(btn.dataset.view));
  }

  let searchTimer = null;
  $('searchInput').addEventListener('input', (e) => {
    const q = e.target.value.trim();
    clearTimeout(searchTimer);
    searchTimer = setTimeout(async () => {
      if (!q) {
        if (state.view === 'search') await selectView('all');
        return;
      }
      state.view = 'search';
      state.searchQuery = q;
      await loadCurrentViewEvents();
      renderSidebar();
      renderContent();
    }, 150);
  });

  applyThemeAndLang();
  void loadAll();
  connectSSE();
  setInterval(() => {
    apiGet('/ui/api/status').then((st) => {
      state.status = st;
      renderSidebar();
      renderSovereignBar();
    }).catch(() => {});
  }, 5000);
})();
