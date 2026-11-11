// Remote status has its own queue/cache; it never blocks or repaints local diffs.
export function createWorktreePRController({ load, onChange, now = Date.now, cacheMs = 120000, concurrency = 2 }) {
  const entries = new Map();
  let paths = new Set(), running = 0, enabled = false;
  const queue = [];
  function pump() {
    while (enabled && running < concurrency && queue.length) {
      const { path, entry, request, force } = queue.shift();
      if (request.signal.aborted || !paths.has(path)) continue;
      running++;
      Promise.resolve().then(() => load(path, force, request.signal)).then(value => {
        if (!request.signal.aborted && entry.request === request) {
          entry.value = value; entry.error = ''; entry.updatedAt = now();
        }
      }).catch(error => {
        if (!request.signal.aborted && entry.request === request) { entry.error = error.message; entry.updatedAt = now(); }
      }).finally(() => {
        running--;
        if (entry.request === request) { entry.request = null; entry.loading = false; onChange(); }
        pump();
      });
    }
  }
  function refresh(path, force = false) {
    if (!enabled || !paths.has(path)) return;
    let entry = entries.get(path);
    if (!entry) { entry = { value: null, error: '', updatedAt: null, request: null, loading: false }; entries.set(path, entry); }
    if (entry.request || !force && entry.updatedAt !== null && now() - entry.updatedAt < cacheMs) return;
    const request = new AbortController(); entry.request = request; entry.loading = true;
    queue.push({ path, entry, request, force }); pump();
  }
  return {
    entries, refresh,
    sync(nextPaths, visible) {
      paths = new Set(nextPaths); enabled = visible;
      for (const [path, entry] of entries) {
        if (!enabled || !paths.has(path)) { entry.request?.abort(); entry.request = null; entry.loading = false; }
        if (!paths.has(path)) entries.delete(path);
      }
      if (!enabled) queue.length = 0;
      else for (const path of paths) refresh(path);
      pump();
    },
    refreshAll(force = false) { for (const path of paths) refresh(path, force); },
  };
}

export function reviewLabel(pr, stale = false) {
  if (stale) return 'Review status unavailable';
  if (pr.reviewState === 'approved' && pr.reviewRequirements === 'satisfied') return 'Approval received';
  if (pr.reviewState === 'approved') return 'Review received';
  if (pr.reviewState === 'reviewed') return 'Review received';
  if (pr.reviewState === 'requested') return 'Reviewer requested';
  return 'No reviewer requested';
}

export function checksLabel(pr) {
  const checks = pr.checks || [], failed = checks.filter(c => c.state === 'failed').length;
  if (pr.checksState === 'failed') return failed + (failed === 1 ? ' failed check' : ' failed checks');
  if (pr.checksState === 'pending') return 'Checks running';
  if (pr.checksState === 'passed') return checks.some(c => c.state === 'passed') ? 'All checks passed' : 'Checks skipped';
  return 'No checks';
}

export function safeCheckURL(raw) {
  try { const u = new URL(raw); return u.protocol === 'https:' && !u.username && !u.password ? u.href : ''; } catch { return ''; }
}

// An empty lookup is only a no-PR result when GitHub actually answered.
export function worktreePRNotice(entry) {
  if (entry?.error) return { error: entry.error, message: '' };
  if (entry?.loading) return { error: '', message: 'Checking GitHub…' };
  if (entry?.value?.state === 'none') return { error: '', message: 'No open PR has been raised for this branch.' };
  return { error: '', message: entry?.value?.reason || 'PR status has not been checked yet.' };
}
