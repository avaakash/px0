import { $, S } from './state.js';
import { on } from './bus.js';
import { openFile, switchTab } from './tabs.js';
import { getMode, getLayout, setMode, setLayout } from './reviewflow.js';
import { worktreeSession, prepareWorktreeSession, waitForWorktreeSession } from './worktreechanges.js';
import { terminalSession, restoreTerminalSession } from './terminal.js';
import { inGitMode, setSidebarMode, openDirs, refreshTree } from './tree.js';
import { showRightInspector, hideRightInspector } from './inspector.js';
import { layout, render } from './renderer.js';
import { createWorkspaceSessionStore, sessionNumber } from './workspace-session-state.js';

let store, ready = false, timer, previous = '';
const scroll = selector => ({ top: $(selector)?.scrollTop || 0, left: $(selector)?.scrollLeft || 0 });
function snapshot() {
  const viewport = scroll('#viewport'), diff = scroll('#diffview');
  return {
    mode: getMode(), reviewLayout: getLayout(), worktrees: worktreeSession(), terminal: terminalSession(),
    tabs: S.tabs.slice(0, 50).map((tab, index) => ({ path: tab.path, ref: tab.diffRef || '', base: tab.diffBase || '',
      line: tab.cur || 1, view: tab.diffMode ? 'diff' : 'source',
      scroll: index === S.active ? viewport : { top: tab.scrollTop || 0, left: 0 },
      diffScroll: index === S.active ? diff : { top: tab.diffScroll || 0, left: 0 },
    })), activeFile: S.tabs[S.active]?.path || '',
    sidebar: { mode: inGitMode() ? 'git' : 'files', hidden: document.body.classList.contains('side-hidden'),
      width: parseFloat($('#side').style.width) || 0, dirs: [...openDirs].slice(0, 300), scroll: scroll('#tree').top },
    inspector: { hidden: document.body.classList.contains('right-hidden'), width: parseFloat($('#right-side').style.width) || 0,
      tab: document.querySelector('.inspector-tab.active')?.dataset.itab || '' },
    gitPanel: { collapsed: $('#git-panel').classList.contains('collapsed'), height: parseFloat($('#git-panel').style.height) || 0 },
  };
}
function save() {
  clearTimeout(timer);
  if (!ready) return;
  const value = snapshot(), signature = JSON.stringify(value);
  if (signature !== previous) { store.write(value); previous = signature; }
}
function scheduleSave() { if (ready) { clearTimeout(timer); timer = setTimeout(save, 200); } }
export function loadWorkspaceSession(root) {
  let storage; try { storage = localStorage; } catch {}
  store = createWorkspaceSessionStore(storage, root);
  return store.read();
}
export async function restoreWorkspaceSession(saved, { initialPath = false } = {}) {
  // Initial URL navigation wins over saved file/mode state.
  if (saved) {
    for (const tab of (!initialPath && Array.isArray(saved.tabs) ? saved.tabs : []).slice(0, 50)) {
      if (typeof tab?.path !== 'string' || !tab.path || tab.path.length > 8192) continue;
      await openFile(tab.path, { push: false, ref: typeof tab.ref === 'string' ? tab.ref : '', base: typeof tab.base === 'string' ? tab.base : '',
        line: sessionNumber(tab.line, 1, 10000000), view: tab.view === 'diff' ? 'diff' : 'source', soft: true });
      const doc = S.tabs.find(t => t.path === tab.path);
      if (doc) { doc.scrollTop = sessionNumber(tab.scroll?.top, 0, 100000000); doc.diffScroll = sessionNumber(tab.diffScroll?.top, 0, 100000000); }
    }
    if (!initialPath) {
      const index = S.tabs.findIndex(t => t.path === saved.activeFile);
      if (index >= 0) switchTab(index);
      const tab = saved.tabs?.find?.(t => t.path === saved.activeFile);
      if (tab) {
        $('#viewport').scrollTop = sessionNumber(tab.scroll?.top, 0, 100000000);
        $('#viewport').scrollLeft = sessionNumber(tab.scroll?.left, 0, 100000000);
        $('#diffview').scrollTop = sessionNumber(tab.diffScroll?.top, 0, 100000000);
        $('#diffview').scrollLeft = sessionNumber(tab.diffScroll?.left, 0, 100000000);
      }
    }
    await setSidebarMode(saved.sidebar?.mode === 'git' ? 'git' : 'files', { expandDirty: false });
    openDirs.clear();
    for (const path of (Array.isArray(saved.sidebar?.dirs) ? saved.sidebar.dirs : []).slice(0, 300)) if (typeof path === 'string') openDirs.add(path);
    await refreshTree();
    $('#tree').scrollTop = sessionNumber(saved.sidebar?.scroll, 0, 100000000);
    document.body.classList.toggle('side-hidden', saved.sidebar?.hidden === true);
    if (saved.sidebar?.width > 0) $('#side').style.width = sessionNumber(saved.sidebar.width, 170, 620) + 'px';
    const inspector = saved.inspector;
    if (inspector?.width > 0) $('#right-side').style.width = sessionNumber(inspector.width, 200, 700) + 'px';
    const tab = ['refs', 'symbols', 'calls', 'search', 'threads', 'tips'].includes(inspector?.tab) ? inspector.tab : undefined;
    showRightInspector(tab);
    if (inspector?.hidden) hideRightInspector();
    $('#git-panel').classList.toggle('collapsed', saved.gitPanel?.collapsed === true);
    if (saved.gitPanel?.height > 0) $('#git-panel').style.height = sessionNumber(saved.gitPanel.height, 60, innerHeight * 0.8) + 'px';
    setLayout(saved.reviewLayout);
    prepareWorktreeSession(saved.worktrees);
    if (!initialPath) setMode(saved.mode);
    if (getMode() === 'worktrees') await waitForWorktreeSession();
  }
  await restoreTerminalSession(saved?.terminal);
  layout(); render(); ready = true;
  for (const event of ['mode:changed', 'tab:activated', 'tab:closed', 'layout:changed', 'sidebar:mode']) on(event, scheduleSave);
  for (const event of ['click', 'change', 'input', 'pointerup', 'mouseup', 'keyup', 'scroll']) document.addEventListener(event, scheduleSave, { capture: true, passive: true });
  addEventListener('pagehide', save); addEventListener('beforeunload', save);
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'hidden') save(); });
  save();
}
