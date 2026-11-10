import { $, S, api, apiPostJson, esc } from './state.js';
import { on, emit } from './bus.js';
import { createWorktreeChangesController } from './worktreechanges-state.js';
import { worktreeDiffHTML } from './worktreechanges-render.js';
import { showToast } from './ui.js';

const WORKTREE_PREFS_KEY = 'px0.worktreeView.v2';
const WORKTREE_INVENTORY_KEY = 'px0.worktreeInventory.v2';
let worktreeReviewVisible = false, worktreeChoiceRevision = 0, worktreeRequestedPath = '';
let worktreeChoices = [], worktreeRepositories = [], worktreeRenderedList = '', worktreeRenderedDiff = '', worktreeLayout = 'split';
let worktreeLineLimit = 2000, worktreeShownSelection = '', worktreeRestoringScroll = false, worktreeScrollRevision = 0;
const worktreeFileLimits = new Map();
let worktreeShowHidden = false;
function worktreeStored(key, fallback) {
  try { return JSON.parse(localStorage.getItem(key)) || fallback; } catch { return fallback; }
}
function saveWorktreeValue(key, value) { try { localStorage.setItem(key, JSON.stringify(value)); } catch {} }
const worktreePreferences = worktreeStored(WORKTREE_PREFS_KEY, {});
const reviewScroll = () => {
  const el = $('#wt-diff');
  if (el && worktreeReviewVisible && !worktreeRestoringScroll) worktreeReview.rememberScroll(el.scrollTop, el.scrollLeft);
};
const worktreeReview = createWorktreeChangesController({
  hidden: Array.isArray(worktreePreferences.hidden) ? worktreePreferences.hidden.filter(p => typeof p === 'string').slice(0, 500) : [],
  expanded: Array.isArray(worktreePreferences.expanded) ? worktreePreferences.expanded.filter(p => typeof p === 'string').slice(0, 100) : [],
  names: worktreePreferences.names,
  onPreferences: value => saveWorktreeValue(WORKTREE_PREFS_KEY, value),
  loadChanges: (worktree, signal) => api('/api/git/worktrees/changes', { worktree }, { signal }),
  loadDiff: (worktree, file, signal) => api('/api/git/worktrees/diff', { worktree, path: file.path, group: file.group, from: file.from }, { signal }),
  mutateChanges: (worktree, group, file) => apiPostJson('/api/git/worktrees/' + (group === 'staged' ? 'unstage' : 'stage'), { worktree, group, path: file?.path || '', from: file?.from || '', all: !file }),
  onChange: renderWorktreeChanges,
});
export const worktreeLabel = w => worktreeReview.label(w);

function renameWorktree(path) {
  const w = worktreeChoices.find(w => w.path === path);
  if (!w) return;
  const dialog = $('#wt-rename-dialog'), input = $('#wt-rename-input');
  dialog.dataset.worktree = path;
  input.value = worktreeLabel(w);
  input.placeholder = w.branch || 'Worktree display name';
  $('#wt-rename-original').textContent = (w.branch ? 'Branch: ' + w.branch + '\n' : '') + w.path;
  $('#wt-rename-reset').disabled = !worktreeReview.state().names.has(path);
  dialog.showModal(); input.focus(); input.select();
}

export function openWorktreeChanges(path = '') {
  worktreeRequestedPath = path;
  const alreadyVisible = worktreeReviewVisible;
  emit('mode:request', 'worktrees');
  if (alreadyVisible) refreshWorktreeChoices();
}

function setWorktreeMessage(message = '') {
  const status = $('#wt-status'); status.textContent = message; status.hidden = !message;
}

async function refreshWorktreeChoices(force = false) {
  const revision = ++worktreeChoiceRevision;
  try {
    const result = await api('/api/git/worktrees', { all: 1, refresh: force ? 1 : undefined });
    if (revision !== worktreeChoiceRevision || !worktreeReviewVisible) return;
    worktreeChoices = result.worktrees || [];
    worktreeRepositories = result.repositories || [];
    saveWorktreeValue(WORKTREE_INVENTORY_KEY, { worktrees: worktreeChoices, repositories: worktreeRepositories });
    worktreeReview.setWorktrees(worktreeChoices);
    if (!worktreeChoices.length) {
      setWorktreeMessage(result.reason || 'No registered worktrees.');
      $('#wt-diff').textContent = result.reason || 'Open a Git repository to review its worktrees.';
    }
    const requested = worktreeRequestedPath; worktreeRequestedPath = '';
    if (requested && worktreeChoices.some(w => w.path === requested && w.available)) {
      worktreeReview.restoreWorktree(requested);
      await worktreeReview.selectWorktree(requested);
    } else if (force) worktreeReview.refreshExpanded(true);
  } catch (error) {
    if (revision === worktreeChoiceRevision && worktreeReviewVisible) setWorktreeMessage(error.message);
  }
}

function worktreeFilesHTML(w, state) {
  const view = state.views.get(w.path);
  if (!view?.loaded) return '<p class="wt-empty">' + esc(view?.error || (view?.loadingFiles ? 'Loading changes…' : 'Expand to load changes.')) + '</p>';
  const query = $('#wt-filter').value.toLowerCase();
  const files = view.files.filter(f => f.path.toLowerCase().includes(query));
  let html = '', remaining = worktreeFileLimits.get(w.path) || 100;
  for (const [group, label] of [['conflicts', 'Merge Changes'], ['staged', 'Staged Changes'], ['unstaged', 'Changes']]) {
    const entries = files.filter(f => f.group === group);
    if (!entries.length) continue;
    html += '<h4 class="wt-group"><span>' + label + ' ' + entries.length + '</span><button type="button" class="wt-stage-all" data-wt-action-group="' + group + '" data-wt-root="' + esc(w.path) + '"' + (view.mutating || query ? ' disabled' : '') + ' title="' + (query ? 'Clear the file filter to use group actions' : 'Apply to all files in this change group') + '" aria-label="' + esc((group === 'staged' ? 'Unstage all' : 'Stage all') + ' · ' + label + ' · ' + worktreeLabel(w)) + '">' + (group === 'staged' ? 'Unstage all' : 'Stage all') + '</button></h4>';
    for (const file of entries.slice(0, Math.max(0, remaining))) {
      const key = worktreeReview.key(file), cut = file.path.lastIndexOf('/');
      const selected = state.worktree === w.path && key === state.selected;
      html += '<div class="wt-file-row"><button type="button" class="wt-file' + (selected ? ' selected' : '') + '" data-wt-file="' + esc(key) + '" data-wt-root="' + esc(w.path) + '" title="' + esc(file.path) + '" aria-label="' + esc(file.path + ' · ' + label + ' · ' + worktreeLabel(w)) + '" aria-pressed="' + selected + '"><span class="wt-file-label"><strong>' + esc(file.path.slice(cut + 1)) + '</strong><small>' + esc(file.path.slice(0, Math.max(0, cut))) + '</small></span><span class="wt-status-' + esc(file.status) + '">' + esc(file.status) + '</span></button><button type="button" class="wt-stage-file" data-wt-action-file="' + esc(key) + '" data-wt-root="' + esc(w.path) + '"' + (view.mutating ? ' disabled' : '') + ' aria-label="' + esc((group === 'staged' ? 'Unstage ' : 'Stage ') + file.path + ' · ' + worktreeLabel(w)) + '" title="' + (group === 'staged' ? 'Unstage file' : 'Stage file') + '">' + (view.mutating ? '…' : group === 'staged' ? '−' : '+') + '</button></div>';
    }
    remaining -= entries.length;
  }
  if (files.length > (worktreeFileLimits.get(w.path) || 100)) html += '<button type="button" class="wt-more" data-wt-more="' + esc(w.path) + '">Show 100 more files</button>';
  return html || '<p class="wt-empty">' + (view.error ? esc(view.error) : query ? 'No matching files.' : 'No uncommitted changes.') + '</p>';
}

function renderWorktreeChanges(state) {
  if (!worktreeReviewVisible) return;
  const query = $('#wt-filter').value;
  const listSignature = JSON.stringify([state.treeRevision, query, worktreeShowHidden, [...worktreeFileLimits]]);
  if (listSignature !== worktreeRenderedList) {
    worktreeRenderedList = listSignature;
    let html = '', repository = null;
    for (const w of state.inventory) {
      if (state.hidden.has(w.path)) continue;
      if (w.repository !== repository) {
        repository = w.repository;
        const repo = worktreeRepositories.find(r => r.path === repository);
        html += '<h3 class="wt-repository" title="' + esc(repository || '') + '">' + esc(repo?.name || 'Repository') + '</h3>';
      }
      const open = state.expanded.has(w.path), view = state.views.get(w.path), label = worktreeLabel(w);
      html += '<section class="wt-card"><header class="wt-card-head"><button type="button" class="wt-card-toggle" data-wt-toggle="' + esc(w.path) + '" aria-expanded="' + open + '"' + (!w.available ? ' disabled' : '') + ' title="' + esc((w.branch ? w.branch + '\n' : '') + w.path) + '"><span class="wt-chevron">' + (open ? '▾' : '▸') + '</span><span class="wt-branch">' + esc(label) + '</span><span class="wt-card-count">' + (view?.loadingFiles ? '…' : view?.loaded ? view.files.length : !w.available ? 'unavailable' : '') + '</span></button><button type="button" class="wt-terminal" data-wt-terminal="' + esc(w.path) + '" aria-label="' + esc('Open terminal in ' + label) + '" title="Open terminal">›_</button><button type="button" class="wt-rename" data-wt-rename="' + esc(w.path) + '" aria-label="' + esc('Rename ' + label) + '" title="Rename display name">✎</button><button type="button" class="wt-hide" data-wt-hide="' + esc(w.path) + '" aria-label="' + esc('Hide ' + label + ' from view') + '" title="Hide from view (keeps files and branch)">×</button></header>' + (open ? '<div class="wt-card-files">' + worktreeFilesHTML(w, state) + '</div>' : '') + '</section>';
    }
    const hidden = state.inventory.filter(w => state.hidden.has(w.path));
    $('#wt-files').innerHTML = html || '<p class="wt-empty">' + (hidden.length ? 'All worktrees are hidden. Restore them below.' : 'Loading worktrees…') + '</p>';
    $('#wt-hidden-toggle').hidden = !hidden.length;
    $('#wt-hidden-toggle').textContent = 'Hidden worktrees (' + hidden.length + ')';
    $('#wt-hidden-toggle').setAttribute('aria-expanded', String(worktreeShowHidden));
    $('#wt-hidden-list').hidden = !worktreeShowHidden || !hidden.length;
    $('#wt-hidden-list').innerHTML = hidden.map(w => '<button type="button" class="wt-restore" data-wt-restore="' + esc(w.path) + '">Restore ' + esc(worktreeLabel(w)) + '</button>').join('') + (hidden.length ? '<button type="button" class="wt-restore" data-wt-restore-all>Restore all</button>' : '');
  }
  setWorktreeMessage();
  const selectedWorktree = state.inventory.find(w => w.path === state.worktree);
  const contextWorktree = selectedWorktree || state.inventory.find(w => w.available && !state.hidden.has(w.path) && state.expanded.has(w.path)) || state.inventory.find(w => w.available && !state.hidden.has(w.path));
  if (contextWorktree) emit('worktree:context', { root: contextWorktree.path, label: worktreeLabel(contextWorktree) });
  const file = state.files.find(f => worktreeReview.key(f) === state.selected);
  $('#wt-file-path').textContent = file ? file.path : 'Worktree Changes';
  $('#wt-file-group').textContent = file ? worktreeLabel(selectedWorktree || {}) + ' · ' + (file.group === 'staged' ? 'HEAD → index' : file.status === 'U' ? 'New file' : 'Index → working tree') : '';
  const id = state.worktree + '\0' + state.selected;
  if (worktreeShownSelection !== id) { worktreeShownSelection = id; worktreeLineLimit = 2000; }
  const signature = JSON.stringify([id, state.diff?.diff, worktreeLayout, worktreeLineLimit, state.error]);
  if (signature === worktreeRenderedDiff) return;
  worktreeRenderedDiff = signature;
  const container = $('#wt-diff'), position = { ...(state.positions.get(state.selected) || { top: 0, left: 0 }) };
  const revision = ++worktreeScrollRevision; worktreeRestoringScroll = true;
  if (state.error) container.textContent = state.error;
  else if (!file) container.innerHTML = '<div class="wt-empty-state"><strong>Select a changed file</strong><p>Expand any worktree on the left to review its staged and unstaged changes.</p></div>';
  else if (!state.diff) container.textContent = 'Loading diff…';
  else if (state.diff.hunks?.length) {
    const result = worktreeDiffHTML(state.diff.hunks, worktreeLayout, worktreeLineLimit);
    container.innerHTML = result.html + (result.remaining ? '<button type="button" class="wt-more" data-wt-lines>Show 2,000 more diff lines (' + result.remaining + ' remaining)</button>' : '');
  } else {
    const pre = document.createElement('pre'); pre.className = 'wt-raw'; pre.textContent = state.diff.diff || 'No text diff (the file may have changed again).'; container.replaceChildren(pre);
  }
  container.scrollTop = position.top; container.scrollLeft = position.left;
  requestAnimationFrame(() => { if (revision !== worktreeScrollRevision) return; container.scrollTop = position.top; container.scrollLeft = position.left; worktreeRestoringScroll = false; });
}

export function initWorktreeChanges() {
  on('mode:changed', mode => {
    worktreeReviewVisible = mode === 'worktrees';
    if (worktreeReviewVisible) {
      if (!worktreeChoices.length) {
        const cached = worktreeStored(WORKTREE_INVENTORY_KEY, {});
        if (Array.isArray(cached.worktrees)) {
          worktreeChoices = cached.worktrees.filter(w => typeof w.path === 'string');
          worktreeRepositories = Array.isArray(cached.repositories) ? cached.repositories : [];
          worktreeReview.setWorktrees(worktreeChoices);
        }
      } else renderWorktreeChanges(worktreeReview.state());
      refreshWorktreeChoices();
    } else { worktreeChoiceRevision++; worktreeReview.pause(); }
  });
  $('#wt-refresh').addEventListener('click', () => { reviewScroll(); refreshWorktreeChoices(true); });
  $('#wt-filter').addEventListener('input', () => { worktreeRenderedList = ''; renderWorktreeChanges(worktreeReview.state()); });
  $('#wt-files').addEventListener('click', e => {
    const terminal = e.target.closest('[data-wt-terminal]'), actionFile = e.target.closest('[data-wt-action-file]'), actionGroup = e.target.closest('[data-wt-action-group]');
    const rename = e.target.closest('[data-wt-rename]'), hide = e.target.closest('[data-wt-hide]'), toggle = e.target.closest('[data-wt-toggle]'), file = e.target.closest('[data-wt-file]'), more = e.target.closest('[data-wt-more]');
    reviewScroll();
    if (terminal) { const w = worktreeChoices.find(w => w.path === terminal.dataset.wtTerminal); if (w) emit('terminal:open', { root: w.path, label: worktreeLabel(w) }); }
    else if (actionFile || actionGroup) {
      const action = actionFile || actionGroup, path = action.dataset.wtRoot;
      const item = actionFile ? worktreeReview.state().views.get(path)?.files.find(f => worktreeReview.key(f) === action.dataset.wtActionFile) : null;
      if (actionGroup || item) worktreeReview.mutateWorktree(path, item?.group || action.dataset.wtActionGroup, item).catch(error => showToast('!', error.message));
    }
    else if (rename) renameWorktree(rename.dataset.wtRename);
    else if (hide) worktreeReview.hideWorktree(hide.dataset.wtHide);
    else if (toggle) worktreeReview.toggleWorktree(toggle.dataset.wtToggle);
    else if (file) worktreeReview.selectFile(file.dataset.wtRoot, file.dataset.wtFile);
    else if (more) { const path = more.dataset.wtMore; worktreeFileLimits.set(path, (worktreeFileLimits.get(path) || 100) + 100); renderWorktreeChanges(worktreeReview.state()); }
  });
  $('#wt-hidden-toggle').addEventListener('click', () => { worktreeShowHidden = !worktreeShowHidden; worktreeRenderedList = ''; renderWorktreeChanges(worktreeReview.state()); });
  $('#wt-hidden-list').addEventListener('click', e => { const restore = e.target.closest('[data-wt-restore]'); if (restore) worktreeReview.restoreWorktree(restore.dataset.wtRestore); else if (e.target.closest('[data-wt-restore-all]')) worktreeReview.restoreAll(); });
  const renameDialog = $('#wt-rename-dialog');
  renameDialog.addEventListener('keydown', e => e.stopPropagation());
  $('#wt-rename-cancel').addEventListener('click', () => renameDialog.close());
  $('#wt-rename-form').addEventListener('submit', e => {
    e.preventDefault(); worktreeReview.renameWorktree(renameDialog.dataset.worktree, $('#wt-rename-input').value); renameDialog.close();
  });
  $('#wt-rename-reset').addEventListener('click', () => { worktreeReview.renameWorktree(renameDialog.dataset.worktree, ''); renameDialog.close(); });
  $('#wt-diff').addEventListener('scroll', reviewScroll, { passive: true });
  $('#wt-diff').addEventListener('click', e => { if (e.target.closest('[data-wt-lines]')) { worktreeLineLimit += 2000; renderWorktreeChanges(worktreeReview.state()); } });
  $('#wt-layout').addEventListener('change', e => { reviewScroll(); worktreeLayout = e.target.value; renderWorktreeChanges(worktreeReview.state()); });
  setInterval(() => { if (worktreeReviewVisible && document.visibilityState === 'visible') { reviewScroll(); worktreeReview.refreshDiff(); } }, 2000);
  setInterval(() => { if (worktreeReviewVisible && document.visibilityState === 'visible') worktreeReview.refreshExpanded(); }, 10000);
}
