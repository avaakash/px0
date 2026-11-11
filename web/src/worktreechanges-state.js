// Independent worktree cards and one active diff. Collapsed cards never run Git.
export function createWorktreeChangesController({ loadChanges, loadDiff, mutateChanges, onChange, onPreferences = () => {}, hidden = [], expanded = [], collapsedGroups = [], names = {}, now = Date.now, cacheMs = 10000, concurrency = 2 }) {
  let inventory = [], active = '', treeRevision = 0, running = 0, diffRevision = 0, diffRequest = null, selectionRevision = 0;
  const views = new Map(), hiddenPaths = new Set(hidden), expandedPaths = new Set(expanded), queue = [], diffLRU = [];
  const collapsedGroupKeys = new Set((Array.isArray(collapsedGroups) ? collapsedGroups : [])
    .filter(key => typeof key === 'string' && /\0(conflicts|staged|unstaged)$/.test(key)).slice(0, 1500));
  const displayNames = new Map(Object.entries(names && typeof names === 'object' && !Array.isArray(names) ? names : {})
    .filter(([path, name]) => path && typeof name === 'string' && name.trim()).slice(0, 500).map(([path, name]) => [path, name.trim().slice(0, 120)]));
  const key = f => f ? f.group + ':' + f.path : '';
  const fileSnapshot = files => JSON.stringify(files.map(f => [key(f), f.status || '', f.from || '']).sort((a, b) => a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  const viewFor = path => {
    if (!views.has(path)) views.set(path, { files: [], selected: '', diff: null, positions: new Map(), loading: false, loadingFiles: false, loaded: false, updatedAt: 0, error: '', request: null });
    return views.get(path);
  };
  const state = () => ({ inventory, views, hidden: hiddenPaths, expanded: expandedPaths, collapsedGroups: collapsedGroupKeys, names: displayNames, treeRevision, worktree: active, ...(active ? viewFor(active) : { files: [], selected: '', diff: null, positions: new Map(), loading: false, error: '' }) });
  const publish = (tree = false) => { if (tree) treeRevision++; onChange(state()); };
  const persist = () => onPreferences({ hidden: [...hiddenPaths], expanded: [...expandedPaths], collapsedGroups: [...collapsedGroupKeys], names: Object.fromEntries(displayNames) });
  const available = path => inventory.some(w => w.path === path && w.available) && !hiddenPaths.has(path);
  function pump() {
    while (running < concurrency && queue.length) {
      const task = queue.shift();
      if (task.signal.aborted) { task.resolve(); continue; }
      running++;
      Promise.resolve().then(task.run).then(task.resolve, task.reject).finally(() => { running--; pump(); });
    }
  }
  function reconcileWorktreeFiles(path, files) {
    const view = viewFor(path), previous = view.files.find(f => key(f) === view.selected);
    const next = files.find(f => key(f) === view.selected) || (previous && files.find(f => f.path === previous.path || f.from === previous.path));
    view.files = files;
    const selected = next ? key(next) : '';
    if (selected !== view.selected) {
      if (selected && view.positions.has(view.selected) && !view.positions.has(selected)) {
        view.positions.set(selected, view.positions.get(view.selected));
        if (view.positions.size > 200) view.positions.delete(view.positions.keys().next().value);
      }
      view.selected = selected; view.diff = null; view.error = ''; view.loading = false;
      if (active === path) { diffRequest?.abort(); diffRevision++; }
    }
  }
  async function ensureChanges(path, force = false) {
    const view = viewFor(path);
    if (!available(path) || !expandedPaths.has(path) || view.mutating) return;
    if (view.pending) {
      const pending = view.pending;
      await pending;
      if (!force || !available(path) || !expandedPaths.has(path) || view.mutating) return;
      // A scan started before an external Git operation may return its old
      // snapshot. Forced recovery needs a scan started after this request.
      if (view.pending && view.pending !== pending) return view.pending;
      return ensureChanges(path, true);
    }
    if (!force && view.loaded && now() - view.updatedAt < cacheMs) return;
    const request = new AbortController(); view.request = request; view.loadingFiles = true; view.error = ''; publish(true);
    const job = new Promise((resolve, reject) => { queue.push({ signal: request.signal, resolve, reject, run: async () => {
      const result = await loadChanges(path, request.signal);
      if (request.signal.aborted || view.request !== request) return;
      reconcileWorktreeFiles(path, result.files || []); view.updatedAt = now(); view.loaded = true;
    } }); pump(); });
    view.pending = job;
    try { await job; }
    catch (error) { if (!request.signal.aborted) view.error = error.message; }
    finally { if (view.request === request) { view.request = null; view.pending = null; view.loadingFiles = false; publish(true); } }
  }
  async function recoverWorktreeSelection(path, request, selectedAt) {
    const view = viewFor(path);
    view.recovery = request; view.diff = null; view.error = ''; view.loading = false; publish();
    try {
      await ensureChanges(path, true);
      if (view.recovery === request && selectedAt === selectionRevision && active === path && available(path) && expandedPaths.has(path) && !view.mutating && !view.error && view.selected) {
        await selectFile(path, view.selected, true, false);
      }
    } finally {
      if (view.recovery === request) view.recovery = null;
      publish();
    }
  }
  async function selectFile(path, id, background = false, recover = true) {
    if (!background) selectionRevision++;
    const selectedAt = selectionRevision;
    if (!available(path)) return;
    const view = viewFor(path), file = view.files.find(f => key(f) === id), filesAtRequest = view.files;
    if (!file) return;
    diffRequest?.abort(); const request = new AbortController(), revision = ++diffRevision; diffRequest = request;
    const changed = active !== path || view.selected !== id;
    active = path;
    if (view.selected !== id) view.diff = null;
    view.selected = id; view.loading = true; view.error = ''; publish(changed);
    try {
      const result = await loadDiff(path, file, request.signal);
      if (request.signal.aborted || revision !== diffRevision || active !== path) return;
      if (recover && expandedPaths.has(path) && selectedAt === selectionRevision && view.files === filesAtRequest && Array.isArray(result.files) && Array.isArray(result.paths)) {
        const paths = new Set(result.paths);
        if (fileSnapshot(view.files.filter(f => paths.has(f.path) || paths.has(f.from))) !== fileSnapshot(result.files)) {
          await recoverWorktreeSelection(path, request, selectedAt);
          return;
        }
      }
      view.diff = result; view.loading = false;
      const at = diffLRU.indexOf(path); if (at >= 0) diffLRU.splice(at, 1); diffLRU.push(path);
      if (diffLRU.length > 6) viewFor(diffLRU.shift()).diff = null;
      publish();
    } catch (error) {
      if (request.signal.aborted || revision !== diffRevision || active !== path || view.selected !== id) return;
      view.loading = false;
      if (recover && expandedPaths.has(path) && error.status === 404 && selectedAt === selectionRevision) {
        await recoverWorktreeSelection(path, request, selectedAt);
        return;
      }
      view.error = error.message; publish();
    }
  }
  function cancelFiles(path) {
    const view = views.get(path);
    if (view) { view.request?.abort(); view.request = null; view.pending = null; view.loadingFiles = false; view.recovery = null; }
  }
  const controller = {
    key, state,
    session() {
      return { active, views: [...views].filter(([path]) => available(path)).slice(0, 100).map(([path, view]) => ({
        path, file: view.files.find(f => key(f) === view.selected) || null,
        positions: [...view.positions].slice(-200),
      })) };
    },
    async restoreSession(saved) {
      const selectedAt = ++selectionRevision;
      for (const item of (Array.isArray(saved?.views) ? saved.views : []).slice(0, 100)) {
        if (!available(item?.path)) continue;
        const view = viewFor(item.path), file = item.file;
        if (Array.isArray(item.positions)) view.positions = new Map(item.positions.slice(-200).filter(p => Array.isArray(p) && typeof p[0] === 'string' && Number.isFinite(p[1]?.top) && Number.isFinite(p[1]?.left)).map(([key, pos]) => [key, { top: Math.max(0, pos.top), left: Math.max(0, pos.left) }]));
        if (file && typeof file.path === 'string' && ['staged', 'unstaged', 'conflicts'].includes(file.group)) {
          const current = view.loaded ? view.files.find(f => key(f) === key(file)) || view.files.find(f => f.path === file.path || f.from === file.path) : file;
          view.selected = key(current);
          if (current && key(current) !== key(file) && view.positions.has(key(file))) view.positions.set(key(current), view.positions.get(key(file)));
          if (!view.loaded) view.files = [{ path: file.path, group: file.group, status: file.status || '', ...(typeof file.from === 'string' ? { from: file.from } : {}) }];
        }
      }
      if (!available(saved?.active)) return;
      const path = saved.active;
      if (expandedPaths.has(path)) await ensureChanges(path);
      if (selectedAt !== selectionRevision || !available(path)) return;
      const view = viewFor(path);
      if (view.selected && view.files.some(f => key(f) === view.selected)) await selectFile(path, view.selected, true);
      else { active = path; publish(true); }
    },
    toggleGroup(path, group) {
      if (!available(path) || !['conflicts', 'staged', 'unstaged'].includes(group)) return;
      const id = path + '\0' + group;
      if (collapsedGroupKeys.has(id)) collapsedGroupKeys.delete(id);
      else if (collapsedGroupKeys.size < 1500) collapsedGroupKeys.add(id);
      persist(); publish(true);
    },
    label(w) { return displayNames.get(w.path) || w.branch || (w.bare ? 'Bare repository' : 'Detached ' + (w.head || '').slice(0, 7)); },
    renameWorktree(path, name) {
      if (!inventory.some(w => w.path === path) || typeof name !== 'string') return;
      const value = name.trim().slice(0, 120);
      if (value) displayNames.set(path, value); else displayNames.delete(path);
      persist(); publish(true);
    },
    setWorktrees(items) {
      inventory = items;
      if (active && !available(active)) { diffRequest?.abort(); diffRevision++; active = ''; }
      for (const path of views.keys()) if (!items.some(w => w.path === path)) { cancelFiles(path); views.delete(path); }
      publish(true);
      for (const path of expandedPaths) if (available(path)) ensureChanges(path);
    },
    async toggleWorktree(path) {
      if (!available(path)) return;
      if (expandedPaths.has(path)) { expandedPaths.delete(path); cancelFiles(path); persist(); publish(true); return; }
      expandedPaths.add(path); persist(); publish(true); await ensureChanges(path);
    },
    async selectWorktree(path) {
      const selectedAt = ++selectionRevision;
      if (!available(path)) return;
      expandedPaths.add(path); persist(); publish(true); await ensureChanges(path);
      if (!available(path) || !expandedPaths.has(path) || selectedAt !== selectionRevision) return;
      const view = viewFor(path), file = view.files.find(f => key(f) === view.selected) || view.files.find(f => f.group === 'unstaged') || view.files[0];
      if (file) await selectFile(path, key(file)); else { active = path; publish(true); }
    },
    selectFile,
    async mutateWorktree(path, group, file = null) {
      if (!available(path) || !mutateChanges) return;
      const view = viewFor(path);
      if (view.mutating) return;
      const selectedAt = selectionRevision, oldFile = view.files.find(f => key(f) === view.selected);
      cancelFiles(path); view.mutating = true; view.error = '';
      if (active === path) { diffRequest?.abort(); diffRevision++; view.loading = false; }
      publish(true);
      try {
        const result = await mutateChanges(path, group, file);
        const touched = new Set(result.paths || []);
        view.files = result.partial ? [...view.files.filter(f => !touched.has(f.path) && !touched.has(f.from)), ...(result.files || [])] : result.files || [];
        view.updatedAt = now(); view.loaded = true;
        if (!view.files.some(f => key(f) === view.selected)) { view.selected = ''; view.diff = null; }
        if (active === path && selectedAt === selectionRevision && available(path)) {
          const next = view.files.find(f => key(f) === view.selected) || view.files.find(f => f.path === oldFile?.path) || view.files[0];
          if (next) await selectFile(path, key(next));
        }
      } catch (error) { view.error = error.message; throw error; }
      finally { view.mutating = false; publish(true); }
    },
    hideWorktree(path) {
      hiddenPaths.add(path); expandedPaths.delete(path); cancelFiles(path);
      if (active === path) { diffRequest?.abort(); diffRevision++; active = ''; }
      persist(); publish(true);
    },
    restoreWorktree(path) { hiddenPaths.delete(path); persist(); publish(true); },
    restoreAll() { hiddenPaths.clear(); persist(); publish(true); },
    async refreshExpanded(force = false) {
      const path = active, selected = path ? viewFor(path).selected : '', selectedAt = selectionRevision;
      await Promise.all([...expandedPaths].filter(available).map(path => ensureChanges(path, force)));
      if (active === path && selectedAt === selectionRevision && path && selected !== viewFor(path).selected) return controller.refreshDiff();
    },
    refreshDiff() { if (active && !viewFor(active).mutating && !viewFor(active).loading && !viewFor(active).recovery && viewFor(active).selected) return selectFile(active, viewFor(active).selected, true); },
    rememberScroll(top, left = 0) {
      if (!active) return; const view = viewFor(active);
      if (view.selected) { view.positions.set(view.selected, { top, left }); if (view.positions.size > 200) view.positions.delete(view.positions.keys().next().value); }
    },
    pause() { selectionRevision++; diffRequest?.abort(); diffRevision++; for (const path of views.keys()) { cancelFiles(path); viewFor(path).loading = false; } },
  };
  return controller;
}
