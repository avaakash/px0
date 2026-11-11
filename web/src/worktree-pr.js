import { $, api, apiPostJson, esc } from './state.js';
import { on, emit } from './bus.js';
import { openSettings } from './settings.js';
import { createWorktreePRController, reviewLabel, checksLabel, safeCheckURL, worktreePRNotice } from './worktree-pr-state.js';

let activePath = '', activeLabel = '', dialogSignature = '', visible = false, inventory = [];
let busy = false, actionMessage = '';
const selected = new Set(), requested = new Map();
const remote = createWorktreePRController({
  load: async (path, force, signal) => {
    try { return await api('/api/git/worktrees/pr', { worktree: path, refresh: force ? 1 : undefined }, { signal }); }
    catch (error) {
      if (!signal.aborted && error.status === 400 && error.message === 'select an available registered worktree of this repository') {
        emit('worktrees:refresh');
        throw new Error('This worktree is no longer available. Refresh the worktree list.');
      }
      throw error;
    }
  },
  onChange: renderPRStatus,
});
const dialog = () => $('#wt-pr-dialog');
const link = (url, text) => {
  const safe = safeCheckURL(url);
  return safe ? '<a href="' + esc(safe) + '" target="_blank" rel="noopener noreferrer">' + esc(text) + '</a>' : esc(text);
};
function isRequested(id) {
  const time = requested.get(activePath + ':' + id);
  return time !== undefined && Date.now() - time < 90000;
}

function renderPRStatus() {
  for (const slot of document.querySelectorAll('[data-wt-pr-slot]')) {
    const path = slot.dataset.wtPrSlot, entry = remote.entries.get(path), pr = entry?.value;
    let html = '';
    if (pr?.state === 'open') {
      const detail = [...(pr.approved || []).map(n => n + ' approved'), ...(pr.changesRequested || []).map(n => n + ' requested changes'), ...(pr.requested || []).map(n => n + ' awaiting review')].join('\n');
      html = '<button type="button" class="wt-pr-summary" data-wt-pr="' + esc(path) + '" title="' + esc(detail || pr.title) + '"><span>PR #' + pr.number + '</span><span class="wt-pr-' + esc(entry.error && pr.reviewState === 'approved' ? 'reviewed' : pr.reviewState) + '">' + esc(reviewLabel(pr, !!entry.error)) + '</span>' + (pr.changesRequested?.length ? '<span class="wt-pr-failed">Changes requested</span>' : '') + '<span class="wt-pr-' + esc(pr.checksState) + '">' + esc(checksLabel(pr)) + '</span>' + (entry.error ? '<span class="wt-pr-failed">Out of date</span>' : '') + '</button>';
    } else if (entry?.error) {
      html = '<button type="button" class="wt-pr-summary wt-pr-failed" data-wt-pr="' + esc(path) + '">GitHub unavailable · Details</button>';
    } else if (pr && pr.state !== 'unsupported') {
      html = '<button type="button" class="wt-pr-summary" data-wt-pr="' + esc(path) + '">' + esc(pr.state === 'none' ? 'PR not raised' : pr.reason || 'PR status unavailable') + '</button>';
    } else if (!pr && entry?.loading) {
      html = '<span class="wt-pr-loading">Checking PR…</span>';
    }
    if (slot.innerHTML !== html) slot.innerHTML = html;
  }
  renderWorktreePRDialog();
}

export function syncWorktreePRs(state, isVisible) {
  inventory = state.inventory.filter(w => w.available && !state.hidden.has(w.path));
  visible = isVisible;
  if (activePath && !inventory.some(w => w.path === activePath) && dialog()?.open && !busy) {
    dialog().close(); activePath = ''; selected.clear();
  }
  remote.sync(inventory.map(w => w.path), visible && document.visibilityState === 'visible');
  renderPRStatus();
}
export function refreshWorktreePRs() { remote.refreshAll(true); }

function openPR(path) {
  activePath = path; activeLabel = inventory.find(w => w.path === path)?.branch || 'Worktree';
  actionMessage = ''; selected.clear(); dialogSignature = '';
  $('#wt-pr-title').textContent = 'Pull request · ' + activeLabel;
  dialog().showModal(); renderWorktreePRDialog(); remote.refresh(path);
}

function renderWorktreePRDialog() {
  if (!dialog()?.open || !activePath) return;
  const entry = remote.entries.get(activePath), pr = entry?.value;
  const signature = JSON.stringify([pr, entry?.error, entry?.loading, busy, [...requested]]);
  if (signature === dialogSignature) { renderActions(); return; }
  dialogSignature = signature;
  let html = '';
  const notice = worktreePRNotice(entry);
  if (notice.error) html += '<p class="wt-pr-error" role="alert">' + esc(notice.error) + '</p>';
  if (pr?.state === 'open') {
    html += '<h3>' + link(pr.url, '#' + pr.number + ' ' + pr.title) + (pr.draft ? ' <small>Draft</small>' : '') + '</h3>';
    html += '<p class="wt-pr-review-state">' + esc(reviewLabel(pr, !!entry.error)) + '</p>';
    if (!entry.error && pr.reviewRequirementsReason) html += '<p class="wt-pr-note">' + esc(pr.reviewRequirementsReason) + '</p>';
    const rows = [['Awaiting review', pr.requested], ['Review received', pr.reviewed], ['Approving reviews', pr.approved], ['Changes requested', pr.changesRequested]];
    html += '<dl class="wt-pr-reviewers">' + rows.filter(([, names]) => names?.length).map(([label, names]) => '<dt>' + label + '</dt><dd>' + esc(names.join(', ')) + '</dd>').join('') + '</dl>';
    html += '<h4>Checks <span class="wt-pr-' + esc(pr.checksState) + '">' + esc(checksLabel(pr)) + '</span></h4>';
    const selectable = new Set();
    for (const check of pr.checks) {
      const waiting = isRequested(check.id), canSelect = check.rerunnable && !waiting && !entry.error;
      if (canSelect) selectable.add(String(check.id));
      html += '<div class="wt-pr-check"><label><input type="checkbox" data-wt-pr-check="' + check.id + '" aria-label="' + esc('Select ' + check.name) + '"' + (!canSelect || busy ? ' disabled' : '') + (selected.has(String(check.id)) && canSelect ? ' checked' : '') + '><span class="wt-pr-' + esc(waiting ? 'pending' : check.state) + '">' + esc(waiting ? 'Rerun requested' : check.state === 'failed' ? check.conclusion || 'failed' : check.state) + '</span></label><span>' + link(check.url, check.name) + '</span></div>';
    }
    for (const id of selected) if (!selectable.has(id)) selected.delete(id);
    if (!pr.checks.length) html += '<p>No checks have been reported.</p>';
    if (pr.checks.some(c => c.state === 'failed' && !c.rerunnable)) html += '<p class="wt-pr-note">Reruns are available for failed GitHub Actions jobs with a connected account. Other checks can be opened using their links.</p>';
    html += '<p class="wt-pr-note">GitHub reports on the PR’s latest pushed commit. A job rerun can also rerun its dependent jobs.</p>';
  } else if (notice.message) {
    html += '<p>' + esc(notice.message) + '</p>';
  }
  if (pr && !pr.connected) html += '<button type="button" id="wt-pr-connect">Connect GitHub</button>';
  $('#wt-pr-body').innerHTML = html;
  $('#wt-pr-close').disabled = busy;
  $('#wt-pr-refresh').disabled = busy || !!entry?.loading;
  $('#wt-pr-rerun').hidden = pr?.state !== 'open';
  renderActions();
}
function renderActions() {
  $('#wt-pr-rerun').disabled = busy || !selected.size || !!remote.entries.get(activePath)?.error;
  $('#wt-pr-rerun').textContent = busy ? 'Requesting reruns…' : 'Rerun selected' + (selected.size ? ' (' + selected.size + ')' : '');
  $('#wt-pr-message').textContent = actionMessage;
}

async function rerunSelected() {
  const entry = remote.entries.get(activePath), pr = entry?.value;
  if (busy || entry?.error || !pr || !selected.size) return;
  busy = true; actionMessage = ''; renderWorktreePRDialog();
  const path = activePath;
  try {
    const reply = await apiPostJson('/api/git/worktrees/pr/rerun', { worktree: path, number: pr.number, headSHA: pr.headSHA, checks: [...selected].map(Number) });
    let accepted = 0; const errors = [];
    for (const result of reply.results) {
      if (result.accepted || result.unconfirmed) { requested.set(path + ':' + result.id, Date.now()); selected.delete(String(result.id)); }
      if (result.accepted) accepted++;
      else errors.push(result.name + ': ' + result.error);
    }
    actionMessage = (accepted ? 'Requested ' + accepted + (accepted === 1 ? ' rerun.' : ' reruns.') : '') + (errors.length ? ' ' + errors.join(' ') : '');
    if (accepted) remote.refresh(path, true);
  } catch (error) { actionMessage = error.message; }
  finally { busy = false; dialogSignature = ''; renderPRStatus(); }
}

export function initWorktreePRs() {
  $('#wt-files').addEventListener('click', e => { const button = e.target.closest('[data-wt-pr]'); if (button) openPR(button.dataset.wtPr); });
  $('#wt-pr-close').addEventListener('click', () => { if (!busy) dialog().close(); });
  dialog().addEventListener('cancel', e => { if (busy) e.preventDefault(); });
  dialog().addEventListener('keydown', e => e.stopPropagation());
  $('#wt-pr-body').addEventListener('change', e => {
    if (!e.target.matches('[data-wt-pr-check]')) return;
    if (e.target.checked && selected.size >= 20) { e.target.checked = false; actionMessage = 'Select up to 20 jobs at a time.'; }
    else if (e.target.checked) selected.add(e.target.dataset.wtPrCheck); else selected.delete(e.target.dataset.wtPrCheck);
    renderActions();
  });
  $('#wt-pr-body').addEventListener('click', e => { if (e.target.closest('#wt-pr-connect')) { dialog().close(); openSettings('ui', 'GitHub', 'github.token'); } });
  $('#wt-pr-refresh').addEventListener('click', () => { remote.refresh(activePath, true); dialogSignature = ''; renderWorktreePRDialog(); });
  $('#wt-pr-rerun').addEventListener('click', rerunSelected);
  $('#wt-github').addEventListener('click', () => openSettings('ui', 'GitHub', 'github.token'));
  document.addEventListener('visibilitychange', () => remote.sync(inventory.map(w => w.path), visible && document.visibilityState === 'visible'));
  on('forge:connected', () => remote.refreshAll(true));
  setInterval(() => {
    if (!visible || document.visibilityState !== 'visible') return;
    for (const [key, at] of requested) if (Date.now() - at > 90000) requested.delete(key);
    remote.refreshAll();
    if (dialog().open && [...requested.keys()].some(key => key.startsWith(activePath + ':'))) remote.refresh(activePath, true);
    renderPRStatus();
  }, 30000);
}
