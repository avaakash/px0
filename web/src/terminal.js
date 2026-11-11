import { $, S, apiPostJson } from './state.js';
import { on, emit } from './bus.js';
import { showToast } from './ui.js';
import { worktreeLabel } from './worktreechanges.js';
import { readTerminalFrames, terminalBytesBase64 } from './terminal-stream.js';
import { terminalRestoreSelection } from './workspace-session-state.js';
import { terminalHeightBounds, terminalHeight, terminalThemeColors } from './terminal-layout.js';

const terminalViews = new Map();
let terminalActiveID = '', terminalOpening = false, terminalAssetsPromise, terminalHeaderSignature = '';
let terminalTarget = { root: '', label: '' };
let terminalPanelHeight = 260, terminalRestorePromise = Promise.resolve(), terminalSelectionRevision = 0;
try { const saved = Number(localStorage.getItem('px0.terminal.height')); if (saved > 0) terminalPanelHeight = saved; } catch {}
function terminalAssets() {
  if (!terminalAssetsPromise) terminalAssetsPromise = (async () => {
    const css = document.createElement('link'); css.rel = 'stylesheet'; css.href = new URL('static/vendor/xterm.css', document.baseURI).href; document.head.append(css);
    for (const path of ['xterm.js', 'xterm-fit.js']) await new Promise((resolve, reject) => {
      const script = document.createElement('script'); script.src = new URL('static/vendor/' + path, document.baseURI).href;
      script.onload = resolve; script.onerror = () => reject(new Error('Could not load terminal assets')); document.head.append(script);
    });
  })().catch(error => { terminalAssetsPromise = null; throw error; });
  return terminalAssetsPromise;
}
function terminalTitle(root) { return terminalViews.get(terminalActiveID)?.root === root ? terminalViews.get(terminalActiveID).label : root.split('/').filter(Boolean).pop() || root; }
function terminalDisplayLabel(view) {
  return view.name || worktreeLabel({ path: view.root, branch: view.label }) + (view.number > 1 ? ' (' + view.number + ')' : '');
}
function renameTerminal(id = terminalActiveID) {
  const view = terminalViews.get(id); if (!view) return;
  const dialog = $('#terminal-rename-dialog'), input = $('#terminal-rename-input');
  dialog.dataset.terminalId = id; input.value = terminalDisplayLabel(view);
  $('#terminal-rename-directory').textContent = view.root;
  $('#terminal-rename-reset').disabled = !view.name;
  dialog.showModal(); input.focus(); input.select();
}
async function saveTerminalName(name) {
  const dialog = $('#terminal-rename-dialog'), view = terminalViews.get(dialog.dataset.terminalId), save = $('#terminal-rename-save');
  if (!view || save.disabled) return;
  save.disabled = true; $('#terminal-rename-reset').disabled = true;
  try {
    const result = await apiPostJson('/api/terminal/rename', { id: view.id, token: view.token, name });
    view.name = result.name; updateTerminalHeader(); dialog.close();
  } catch (error) { showToast('!', error.message); }
  finally { save.disabled = false; $('#terminal-rename-reset').disabled = !view.name; }
}
function updateTerminalHeader() {
  const signature = JSON.stringify([terminalActiveID, [...terminalViews.values()].map(view => [view.id, terminalDisplayLabel(view), !!view.exited, !!view.closing])]);
  if (signature === terminalHeaderSignature) return;
  terminalHeaderSignature = signature;
  const list = $('#terminal-sessions'); list.hidden = terminalViews.size === 0;
  for (const row of list.querySelectorAll('[data-terminal-row]')) if (!terminalViews.has(row.dataset.terminalRow)) row.remove();
  for (const view of terminalViews.values()) {
    const label = terminalDisplayLabel(view);
    let button = $('#terminal-tab-' + view.id);
    if (!button) {
      button = document.createElement('button'); button.type = 'button'; button.className = 'terminal-session';
      button.id = 'terminal-tab-' + view.id; button.dataset.terminalId = view.id;
      const icon = document.createElement('span'); icon.className = 'terminal-session-icon'; icon.textContent = '›_'; icon.setAttribute('aria-hidden', 'true');
      const name = document.createElement('span'); name.className = 'terminal-session-name';
      button.append(icon, name);
      const row = document.createElement('div'); row.className = 'terminal-session-row'; row.dataset.terminalRow = view.id; row.setAttribute('role', 'presentation');
      const rename = document.createElement('button'); rename.type = 'button'; rename.className = 'terminal-session-action'; rename.dataset.terminalRename = view.id; rename.textContent = '✎'; rename.title = 'Rename terminal';
      const remove = document.createElement('button'); remove.type = 'button'; remove.className = 'terminal-session-action'; remove.dataset.terminalDelete = view.id; remove.title = 'Close terminal and stop its shell';
      remove.innerHTML = '<svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.2" aria-hidden="true"><path d="M2 4h12M6 4V2h4v2M4 4l1 10h6l1-10M7 6v6M9 6v6"/></svg>';
      row.append(button, rename, remove); list.append(row);
    }
    button.setAttribute('role', 'tab'); button.setAttribute('aria-selected', String(view.id === terminalActiveID));
    button.setAttribute('aria-controls', view.element.id); button.tabIndex = view.id === terminalActiveID ? 0 : -1;
    button.title = label + '\n' + view.root;
    const row = button.parentElement; row.classList.toggle('selected', view.id === terminalActiveID);
    const rename = row.querySelector('[data-terminal-rename]'), remove = row.querySelector('[data-terminal-delete]');
    rename.setAttribute('aria-label', 'Rename ' + label); remove.setAttribute('aria-label', 'Delete ' + label);
    rename.disabled = !!view.closing; remove.disabled = !!view.closing;
    button.querySelector('.terminal-session-name').textContent = label;
    let status = button.querySelector('small');
    if (view.exited && !status) { status = document.createElement('small'); status.textContent = 'exited'; button.append(status); }
    if (!view.exited) status?.remove();
    view.element.setAttribute('aria-label', label + ' terminal');
    if (terminalViews.size > 1) view.element.setAttribute('aria-labelledby', button.id); else view.element.removeAttribute('aria-labelledby');
  }
  const view = terminalViews.get(terminalActiveID);
  $('#terminal-path').textContent = view?.root || ''; $('#terminal-path').title = view?.root || '';
}
function terminalTheme() {
  const style = getComputedStyle(document.documentElement);
  const value = name => style.getPropertyValue(name).trim();
  const colors = terminalThemeColors({ bg: value('--bg'), fg: value('--fg'), dim: value('--dim'), faint: value('--faint'), selection: value('--sel'), red: value('--err'), green: value('--gi'), yellow: value('--m'), blue: value('--accent-fg'), magenta: value('--k'), cyan: value('--o') });
  $('#terminal-panel').style.setProperty('--terminal-bg', colors.background);
  return colors;
}
function syncTerminalTheme() {
  const colors = terminalTheme();
  for (const view of terminalViews.values()) view.term.options.theme = colors;
}
function sizeTerminalPanel(persist = false) {
  const panel = $('#terminal-panel'), handle = $('#terminal-resize'), available = panel.parentElement.clientHeight;
  const bounds = terminalHeightBounds(available), height = terminalHeight(terminalPanelHeight, available);
  panel.style.height = height + 'px';
  handle.setAttribute('aria-valuemin', String(bounds.min)); handle.setAttribute('aria-valuemax', String(bounds.max)); handle.setAttribute('aria-valuenow', String(height));
  if (persist) { terminalPanelHeight = height; try { localStorage.setItem('px0.terminal.height', String(height)); } catch {} }
}
function dockTerminalPanel(mode) {
  const panel = $('#terminal-panel');
  if (mode === 'worktrees') $('.wt-main').append(panel);
  else $('#main').insertBefore(panel, $('#status'));
  sizeTerminalPanel();
  requestAnimationFrame(() => { const view = terminalViews.get(terminalActiveID); if (view) fitTerminal(view); });
}
function fitTerminal(view) {
  if ($('#terminal-panel').hidden || view.id !== terminalActiveID) return;
  try { view.fit.fit(); } catch {}
}
function selectTerminalView(view, focusTerminal = true) {
  terminalSelectionRevision++;
  terminalActiveID = view.id;
  $('#terminal-panel').hidden = false; sizeTerminalPanel();
  for (const item of terminalViews.values()) { item.element.hidden = item !== view; if (item !== view) item.abort?.abort(); }
  connectTerminal(view);
  updateTerminalHeader(); requestAnimationFrame(() => { fitTerminal(view); if (focusTerminal) view.term.focus(); });
  emit('layout:changed');
}
async function connectTerminal(view) {
  if (view.connecting || view.exited && view.replayed || view.closed || $('#terminal-panel').hidden || view.id !== terminalActiveID) return;
  view.connecting = true; view.abort = new AbortController();
  try {
    const response = await fetch(new URL('api/terminal/stream', document.baseURI), {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: view.id, token: view.token, offset: view.offset }), signal: view.abort.signal,
    });
    if (!response.ok) { const body = await response.json(); if (response.status === 403) { view.exited = true; view.term.writeln('\r\n[Session ended; open a new terminal]'); updateTerminalHeader(); return; } throw new Error(body.error || 'Terminal connection failed'); }
    await readTerminalFrames(response.body.getReader(), async frame => {
      if (frame.truncated) { view.term.reset(); view.term.writeln('[Earlier terminal output was trimmed]'); }
      if (frame.data) await new Promise(resolve => {
        const done = () => { view.abort.signal.removeEventListener('abort', done); resolve(); };
        view.abort.signal.addEventListener('abort', done, { once: true });
        view.term.write(Uint8Array.from(atob(frame.data), c => c.charCodeAt(0)), done);
      });
      if (view.closed) return;
      if (frame.offset !== undefined) view.offset = frame.offset;
      if (frame.exited) { view.exited = true; view.replayed = true; view.term.writeln('\r\n[Shell exited' + (frame.code ? ': ' + frame.code : '') + ']'); updateTerminalHeader(); }
    });
    if (!view.exited && !view.closed) throw new Error('Terminal disconnected; reconnecting');
  } catch (error) {
    if (!view.closed && !view.abort.signal.aborted) { showToast('!', error.message); view.retry = setTimeout(() => connectTerminal(view), 2000); }
  } finally {
    view.connecting = false;
    if (view.abort.signal.aborted && !view.closed && !view.exited && !$('#terminal-panel').hidden && view.id === terminalActiveID) queueMicrotask(() => connectTerminal(view));
  }
}
function queueTerminalInput(view, data, binary = false) {
  if (view.exited || view.closed) return;
  const bytes = binary ? Uint8Array.from(data, c => c.charCodeAt(0)) : new TextEncoder().encode(data);
  if (view.pendingBytes + bytes.length > 262144) { showToast('!', 'Terminal input is busy; paste smaller chunks'); return; }
  view.pendingBytes += bytes.length;
  view.input = view.input.then(async () => {
    for (let i = 0; i < bytes.length; i += 16384) {
      if (view.closed || view.exited) return;
      await apiPostJson('/api/terminal/input', { id: view.id, token: view.token, data: terminalBytesBase64(bytes.subarray(i, i + 16384)) });
    }
  }).catch(error => showToast('!', error.message)).finally(() => { view.pendingBytes -= bytes.length; });
}
async function addTerminalView(info, label = '') {
  if (terminalViews.has(info.id)) return terminalViews.get(info.id);
  await terminalAssets();
  if (terminalViews.has(info.id)) return terminalViews.get(info.id);
  const element = document.createElement('div'); element.className = 'terminal-instance'; element.id = 'terminal-view-' + info.id; element.setAttribute('role', 'tabpanel'); element.hidden = true; $('#terminal-instances').append(element);
  const term = new window.Terminal({ cursorBlink: true, scrollback: 2000, fontSize: 12, fontFamily: 'Menlo, monospace', allowProposedApi: false, allowTransparency: false, theme: terminalTheme() });
  const fit = new window.FitAddon.FitAddon(); term.loadAddon(fit); term.open(element);
  const view = { ...info, label: label || terminalTitle(info.root), term, fit, element, offset: 0, input: Promise.resolve(), pendingBytes: 0, closed: false };
  terminalViews.set(info.id, view);
  // Keep terminal shortcuts inside the terminal, including Ctrl-C, arrows and full-screen tools.
  element.addEventListener('keydown', e => e.stopPropagation());
  term.attachCustomKeyEventHandler(e => {
    if (e.type === 'keydown' && e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey && e.key.toLowerCase() === 'c') {
      e.preventDefault(); queueTerminalInput(view, '\x03'); return false;
    }
    return true;
  });
  term.onData(data => queueTerminalInput(view, data)); term.onBinary(data => queueTerminalInput(view, data, true));
  term.onResize(({ cols, rows }) => { if (!view.exited && !view.closed) apiPostJson('/api/terminal/resize', { id: view.id, token: view.token, cols, rows }).catch(() => {}); });
  view.observer = new ResizeObserver(() => fitTerminal(view)); view.observer.observe(element);
  connectTerminal(view); return view;
}
export async function openTerminal(root = '', label = '', fresh = false) {
  await terminalRestorePromise;
  if (terminalOpening) return;
  terminalOpening = true; $('#terminal-open').disabled = true;
  try {
    const active = terminalViews.get(terminalActiveID);
    root = root || (fresh ? active?.root : '') || terminalTarget.root || S.meta?.root;
    if (!root) throw new Error('Workspace is still loading');
    if (!fresh) {
      const existing = active?.root === root && !active.exited ? active : [...terminalViews.values()].find(v => v.root === root && !v.exited && !v.closed);
      if (existing) { if (label) existing.label = label; selectTerminalView(existing); return; }
    }
    const info = await apiPostJson('/api/terminal/open', { worktree: root, cols: 100, rows: 20, new: fresh });
    const view = await addTerminalView(info, label || (terminalTarget.root === root ? terminalTarget.label : ''));
    if (label) view.label = label;
    selectTerminalView(view);
  } catch (error) { showToast('!', error.message); }
  finally { terminalOpening = false; $('#terminal-open').disabled = false; }
}
async function closeTerminal(id) {
  const view = terminalViews.get(id); if (!view || view.closing) return;
  view.closing = true; updateTerminalHeader();
  try {
    await apiPostJson('/api/terminal/close', { id: view.id, token: view.token });
    const index = [...terminalViews.keys()].indexOf(id);
    view.closed = true; view.abort?.abort(); clearTimeout(view.retry); view.observer.disconnect(); view.term.dispose(); view.element.remove(); terminalViews.delete(id);
    if (terminalActiveID === id) {
      const remaining = [...terminalViews.values()], next = remaining[Math.min(index, remaining.length - 1)];
      if (next) selectTerminalView(next);
      else { terminalActiveID = ''; $('#terminal-panel').hidden = true; updateTerminalHeader(); emit('layout:changed'); }
    } else updateTerminalHeader();
  } catch (error) { showToast('!', error.message); }
  finally { view.closing = false; updateTerminalHeader(); }
}
export function initTerminal() {
  $('#btn-terminal').addEventListener('click', () => openTerminal());
  $('#terminal-hide').addEventListener('click', () => { terminalSelectionRevision++; $('#terminal-panel').hidden = true; for (const view of terminalViews.values()) view.abort?.abort(); emit('layout:changed'); });
  $('#terminal-open').addEventListener('click', () => openTerminal('', '', true));
  $('#terminal-rename-dialog').addEventListener('keydown', e => e.stopPropagation());
  $('#terminal-rename-cancel').addEventListener('click', () => $('#terminal-rename-dialog').close());
  $('#terminal-rename-form').addEventListener('submit', e => { e.preventDefault(); saveTerminalName($('#terminal-rename-input').value); });
  $('#terminal-rename-reset').addEventListener('click', () => saveTerminalName(''));
  $('#terminal-sessions').addEventListener('click', e => {
    const rename = e.target.closest('[data-terminal-rename]'), remove = e.target.closest('[data-terminal-delete]');
    if (rename) { renameTerminal(rename.dataset.terminalRename); return; }
    if (remove) { closeTerminal(remove.dataset.terminalDelete); return; }
    const button = e.target.closest('[data-terminal-id]'), view = button && terminalViews.get(button.dataset.terminalId);
    if (view) selectTerminalView(view);
  });
  $('#terminal-sessions').addEventListener('keydown', e => {
    if (e.key === 'F2') { e.preventDefault(); e.stopPropagation(); renameTerminal(e.target.closest('[data-terminal-row]')?.dataset.terminalRow || terminalActiveID); return; }
    if (!['ArrowUp', 'ArrowDown', 'Home', 'End'].includes(e.key)) return;
    const items = [...terminalViews.values()], at = items.findIndex(v => v.id === terminalActiveID);
    const index = e.key === 'Home' ? 0 : e.key === 'End' ? items.length - 1 : (at + (e.key === 'ArrowUp' ? -1 : 1) + items.length) % items.length;
    e.preventDefault(); e.stopPropagation(); if (items[index]) { selectTerminalView(items[index], false); $('#terminal-tab-' + items[index].id)?.focus(); }
  });
  const resize = $('#terminal-resize'); let drag = null;
  resize.addEventListener('pointerdown', e => {
    if (e.button !== 0) return;
    e.preventDefault(); drag = { y: e.clientY, height: $('#terminal-panel').getBoundingClientRect().height, pointer: e.pointerId };
    resize.setPointerCapture(e.pointerId); document.body.classList.add('terminal-resizing');
  });
  resize.addEventListener('pointermove', e => { if (!drag || drag.pointer !== e.pointerId) return; terminalPanelHeight = drag.height + drag.y - e.clientY; sizeTerminalPanel(); });
  const finishResize = e => { if (!drag || drag.pointer !== e.pointerId) return; drag = null; document.body.classList.remove('terminal-resizing'); sizeTerminalPanel(true); };
  resize.addEventListener('pointerup', finishResize); resize.addEventListener('pointercancel', finishResize); resize.addEventListener('lostpointercapture', finishResize);
  resize.addEventListener('dblclick', () => { terminalPanelHeight = 260; sizeTerminalPanel(true); });
  resize.addEventListener('keydown', e => {
    if (!['ArrowUp', 'ArrowDown', 'Home', 'End'].includes(e.key)) return;
    e.preventDefault(); e.stopPropagation(); const panel = $('#terminal-panel'), bounds = terminalHeightBounds(panel.parentElement.clientHeight);
    terminalPanelHeight = e.key === 'Home' ? bounds.min : e.key === 'End' ? bounds.max : panel.getBoundingClientRect().height + (e.key === 'ArrowUp' ? 20 : -20);
    sizeTerminalPanel(true);
  });
  addEventListener('resize', () => sizeTerminalPanel());
  on('theme:changed', syncTerminalTheme); syncTerminalTheme();
  on('terminal:open', value => openTerminal(value.root, value.label));
  on('worktree:context', value => {
    terminalTarget = value;
    for (const view of terminalViews.values()) if (view.root === value.root && value.label) view.label = value.label;
    updateTerminalHeader();
  });
  on('mode:changed', mode => { dockTerminalPanel(mode); if (mode !== 'worktrees') terminalTarget = { root: S.meta?.root || '', label: S.meta?.name || '' }; });
  addEventListener('beforeunload', () => { for (const view of terminalViews.values()) { view.closed = true; view.abort?.abort(); clearTimeout(view.retry); } });
}

export function terminalSession() {
  return { active: terminalActiveID, visible: !$('#terminal-panel').hidden, height: terminalPanelHeight };
}
export function restoreTerminalSession(saved) {
  const revision = terminalSelectionRevision;
  if (Number.isFinite(saved?.height) && saved.height > 0) terminalPanelHeight = saved.height;
  terminalRestorePromise = (async () => {
    // Reconnect only shells still owned by this px0 process; never spawn a shell
    // from saved browser state, and never persist the server's session tokens.
    const result = await apiPostJson('/api/terminal/list', {});
    for (const info of result.sessions || []) await addTerminalView(info);
    if (revision !== terminalSelectionRevision) return;
    const restore = terminalRestoreSelection([...terminalViews.values()], saved);
    terminalActiveID = restore.active;
    const view = terminalViews.get(terminalActiveID);
    for (const item of terminalViews.values()) item.element.hidden = item !== view;
    if (view && restore.visible) selectTerminalView(view, false);
    else { $('#terminal-panel').hidden = true; updateTerminalHeader(); }
  })().catch(error => showToast('!', 'Could not restore terminals: ' + error.message));
  return terminalRestorePromise;
}
