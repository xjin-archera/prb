/* PR Review Board — small helpers around htmx. No build step. */
const prb = {
  key() { return document.querySelector('#detail .dwrap')?.dataset.key || ''; },
  tab() { return document.querySelector('#tabbody')?.dataset.tab || 'review'; },
  path(key) { const [repo, n] = key.split('#'); return `/pr/${repo}/${n}`; },
  load(key, tab) { htmx.ajax('GET', `${this.path(key)}${tab ? `?tab=${tab}` : ''}`, { target: '#detail', swap: 'innerHTML' }); },
  reloadTab(tab) { const k = this.key(); if (k) htmx.ajax('GET', `${this.path(k)}/tab/${tab || this.tab()}`, { target: '#tabs', swap: 'innerHTML' }); },

  /* author menu: the list response swaps in a fresh (hidden) menu, so remember whether it was open */
  menuOpen: false,
  toggleMenu(ev) {
    ev.stopPropagation();
    const m = document.getElementById('authorMenu'); m.hidden = !m.hidden; this.menuOpen = !m.hidden;
  },
  authors(all) {
    document.querySelectorAll('#authorMenu input[name=author]').forEach((cb) => { cb.checked = all; });
    htmx.trigger('#filters', 'change');
  },

  removeCard(btn) { const ic = btn.closest('.ic'); const tr = ic.closest('tr.crow'); ic.remove(); if (tr && !tr.querySelector('.ic')) tr.remove(); },
  foldAll(closed) { document.querySelectorAll('details.dfile').forEach((d) => { d.open = !closed; }); },
  unfold(id) { const d = document.getElementById(id); if (d) d.open = true; },
  setChat(open) {
    document.body.classList.toggle('chat-open', open);
    const a = document.getElementById('chat'); if (a) a.hidden = !open || !a.innerHTML.trim();
    localStorage.setItem('prb.chatOpen', open ? '1' : '0');
    document.getElementById('toggleChat')?.classList.toggle('active', open);
  },
  openChat() { this.setChat(true); },
  closeChat() { this.setChat(false); },
  toast(msg, err) { const d = document.createElement('div'); d.textContent = msg; if (err) d.className = 'err'; d.dataset.ttl = err ? 8000 : 3500; document.getElementById('toast').appendChild(d); },
};

/* toasts expire */
new MutationObserver((muts) => muts.forEach((m) => m.addedNodes.forEach((n) => {
  if (n.nodeType === 1 && n.dataset?.ttl) setTimeout(() => n.remove(), +n.dataset.ttl);
}))).observe(document.getElementById('toast'), { childList: true });

/* list: selection count, collapse, author menu */
document.getElementById('list').addEventListener('change', (e) => {
  if (!e.target.classList.contains('sel')) return;
  const n = document.querySelectorAll('#list .sel:checked').length;
  const b = document.getElementById('runSelected'); b.disabled = n === 0; b.textContent = n ? `Run ${n} selected` : 'Run selected';
});
document.getElementById('toggleList').addEventListener('click', () => {
  const on = document.body.classList.toggle('list-collapsed'); localStorage.setItem('prb.listCollapsed', on ? '1' : '0');
});
document.addEventListener('click', () => { const m = document.getElementById('authorMenu'); if (m) m.hidden = true; prb.menuOpen = false; });
document.body.addEventListener('htmx:oobAfterSwap', (e) => {
  const m = document.getElementById('authorMenu'); if (m && prb.menuOpen) m.hidden = false;
  if (e.detail.target.id === 'chat') prb.setChat(localStorage.getItem('prb.chatOpen') === '1');
});

/* after swaps: active row, chat toggle wiring, focus a comment in the diff */
document.body.addEventListener('htmx:afterSwap', (e) => {
  const t = e.detail.target;
  if (t.id === 'detail' || t.id === 'list') {
    const k = prb.key();
    document.querySelectorAll('#list .pr').forEach((el) => el.classList.toggle('active', el.dataset.key === k));
  }
  if (t.id === 'detail') {
    document.getElementById('toggleChat')?.addEventListener('click', () => prb.setChat(!document.body.classList.contains('chat-open')));
    prb.setChat(localStorage.getItem('prb.chatOpen') === '1');
    if (!prb.key()) { const a = document.getElementById('chat'); if (a) a.hidden = true; }
  }
  if (t.id === 'chat') { prb.setChat(document.body.classList.contains('chat-open')); const box = document.getElementById('chatMsgs'); if (box) box.scrollTop = box.scrollHeight; }
  if (t.id === 'tabs' || t.id === 'detail') {
    const f = document.querySelector('#tabbody')?.dataset.focus;
    if (f) { const el = document.querySelector(`.ic[data-id="${f}"]`); if (el) { el.scrollIntoView({ block: 'center' }); el.classList.add('flash'); } }
  }
  if (t.classList?.contains('ic') || t.tagName === 'TR') { t.querySelector?.('textarea')?.focus(); }
});

/* SSE: log autoscroll, global status → reload list/detail, chat done → reload the tab when the review changed */
document.body.addEventListener('htmx:sseMessage', (e) => {
  const { type, data } = e.detail;
  if (type === 'line') { const pre = document.getElementById('logpre'); if (pre) { const p = pre.parentElement; if (p.scrollHeight - p.scrollTop - p.clientHeight < 200) p.scrollTop = p.scrollHeight; } }
  if (type === 'text' || type === 'tool') { const live = document.getElementById('live'); if (live) live.hidden = false; const box = document.getElementById('chatMsgs'); if (box) box.scrollTop = box.scrollHeight; }
  if (type === 'status') {
    try {
      const { key, status } = JSON.parse(data);
      if (key !== prb.key()) return;
      if (['done', 'failed', 'posted'].includes(status)) prb.load(key, status === 'done' ? 'review' : prb.tab());
      else if (status === 'running') prb.load(key, 'log');
    } catch {}
  }
  if (type === 'done') {
    try { const d = JSON.parse(data); if (d.updated) { prb.toast('Claude Code updated the review'); prb.reloadTab(); } if (d.error) prb.toast(d.error, true); } catch {}
  }
});

/* disable the button (or the form's submit button) while its request is in flight; CSS draws the spinner */
document.body.addEventListener('htmx:beforeRequest', (e) => {
  const el = e.detail.elt; const btns = el.tagName === 'BUTTON' ? [el] : el.tagName === 'FORM' ? [...el.querySelectorAll('button[type=submit]')] : [];
  btns.forEach((b) => { b.dataset.wasDisabled = b.disabled ? '1' : ''; b.disabled = true; });
});
document.body.addEventListener('htmx:afterRequest', (e) => {
  const el = e.detail.elt; if (!el.isConnected) return;
  const btns = el.tagName === 'BUTTON' ? [el] : el.tagName === 'FORM' ? [...el.querySelectorAll('button[type=submit]')] : [];
  btns.forEach((b) => { b.disabled = b.dataset.wasDisabled === '1'; delete b.dataset.wasDisabled; });
});

/* ⌘/Ctrl+Enter submits the enclosing htmx form */
document.body.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && e.target.tagName === 'TEXTAREA') { const f = e.target.closest('form'); if (f) htmx.trigger(f, 'submit'); }
});

/* errors that did not come back as a toast fragment */
document.body.addEventListener('htmx:responseError', (e) => {
  if (!e.detail.xhr.getResponseHeader('HX-Retarget')) prb.toast(`${e.detail.xhr.status}: ${e.detail.xhr.responseText.slice(0, 200)}`, true);
});

/* initial state */
if (localStorage.getItem('prb.listCollapsed') === '1') document.body.classList.add('list-collapsed');
const params = new URLSearchParams(location.search);
if (params.get('pr')) prb.load(params.get('pr'), params.get('tab') || '');
