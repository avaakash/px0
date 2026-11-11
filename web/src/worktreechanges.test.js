import test from 'node:test';
import assert from 'node:assert/strict';
import { createWorktreeChangesController } from './worktreechanges-state.js';
import { worktreeDiffHTML } from './worktreechanges-render.js';
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
const tick = () => new Promise(r => setImmediate(r));
const files = [{ path: 'same.ts', group: 'staged' }, { path: 'same.ts', group: 'unstaged' }];
const inventory = ['A', 'B', 'C'].map(path => ({ path, available: true }));

for (const [before, after] of [['unstaged', 'staged'], ['staged', 'unstaged']]) {
  test('external ' + (after === 'staged' ? 'staging' : 'unstaging') + ' moves the selected file and refreshes its diff immediately', async () => {
    let group = before, scans = 0;
    const c = createWorktreeChangesController({
      now: () => 0,
      loadChanges: async () => { scans++; return { files: [{ path: 'new.py', group, status: group === 'staged' ? 'A' : 'U' }] }; },
      loadDiff: async (path, file) => {
        if (file.group !== group) { const error = new Error('file is no longer changed in this group'); error.status = 404; throw error; }
        return { diff: group + ' contents' };
      },
      onChange() {},
    });
    c.setWorktrees(inventory); await c.selectWorktree('A');
    group = after; // git add/reset happened in a terminal, outside px0's UI
    await c.refreshDiff();
    assert.equal(c.state().files[0].group, after);
    assert.equal(c.state().selected, after + ':new.py');
    assert.equal(c.state().diff.diff, after + ' contents');
    assert.equal(c.state().error, '');
    assert.equal(scans, 2, 'a stale group must bypass the warm list cache');
  });
}

test('external list refresh follows the selected file into its new group', async () => {
  let group = 'unstaged';
  const c = createWorktreeChangesController({
    loadChanges: async () => ({ files: [{ path: 'new.py', group }] }),
    loadDiff: async (path, file) => ({ diff: file.group }), onChange() {},
  });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  group = 'staged'; await c.refreshExpanded(true);
  assert.equal(c.state().selected, 'staged:new.py');
  assert.equal(c.state().diff.diff, 'staged');
});

test('external partial staging refreshes both groups even while the working diff remains valid', async () => {
  let partial = false, scans = 0;
  const original = [{ path: 'same.py', group: 'unstaged', status: 'M' }];
  const current = () => partial ? [...original, { path: 'same.py', group: 'staged', status: 'M' }] : original;
  const c = createWorktreeChangesController({
    now: () => 0,
    loadChanges: async () => { scans++; return { files: current() }; },
    loadDiff: async () => ({ diff: partial ? 'remaining working edits' : 'original', paths: ['same.py'], files: current() }), onChange() {},
  });
  c.setWorktrees(inventory); await c.selectWorktree('A'); c.rememberScroll(100);
  partial = true; await c.refreshDiff();
  assert.equal(c.state().files.length, 2); assert.equal(c.state().selected, 'unstaged:same.py');
  assert.equal(c.state().diff.diff, 'remaining working edits'); assert.equal(c.state().positions.get('unstaged:same.py').top, 100);
  await c.refreshDiff(); assert.equal(scans, 2, 'unchanged diff polls must not scan the entire checkout');
});

test('an older diff snapshot cannot undo a newer file-list refresh', async () => {
  const response = deferred(); let partial = false, deferDiff = false, scans = 0;
  const original = [{ path: 'same.py', group: 'unstaged', status: 'M' }];
  const c = createWorktreeChangesController({
    loadChanges: async () => { scans++; return { files: partial ? [...original, { path: 'same.py', group: 'staged', status: 'M' }] : original }; },
    loadDiff: async () => deferDiff ? response.promise : { diff: 'original' }, onChange() {},
  });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  deferDiff = true; const pending = c.refreshDiff(); partial = true; await c.refreshExpanded(true);
  response.resolve({ diff: 'older working content', paths: ['same.py'], files: original }); await pending;
  assert.equal(c.state().files.length, 2); assert.equal(scans, 2);
});

test('external staging recovery waits out an older scan and reads a fresh snapshot', async () => {
  const oldScan = deferred(); let group = 'unstaged', scans = 0;
  const c = createWorktreeChangesController({
    loadChanges: async () => ++scans === 2 ? oldScan.promise : { files: [{ path: 'new.py', group }] },
    loadDiff: async (_, file) => {
      if (file.group !== group) throw Object.assign(new Error('file is no longer changed in this group'), { status: 404 });
      return { diff: group };
    }, onChange() {},
  });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  const refresh = c.refreshExpanded(true); await tick();
  group = 'staged'; const recovery = c.refreshDiff(); await tick();
  oldScan.resolve({ files: [{ path: 'new.py', group: 'unstaged' }] });
  await Promise.all([refresh, recovery]);
  assert.equal(scans, 3); assert.equal(c.state().selected, 'staged:new.py');
  assert.equal(c.state().diff.diff, 'staged'); assert.equal(c.state().error, '');
});

test('external staging recovery cannot steal a newer worktree selection', async () => {
  const scan = deferred(); let staged = false, reads = 0;
  const c = createWorktreeChangesController({
    loadChanges: async path => path === 'A' && ++reads > 1 ? scan.promise : { files: [{ path: 'same.py', group: 'unstaged' }] },
    loadDiff: async (path, file) => {
      if (path === 'A' && staged && file.group === 'unstaged') throw Object.assign(new Error('file is no longer changed in this group'), { status: 404 });
      return { diff: path + ':' + file.group };
    }, onChange() {},
  });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  staged = true; const recovery = c.refreshDiff(); await tick();
  await c.selectWorktree('B'); scan.resolve({ files: [{ path: 'same.py', group: 'staged' }] }); await recovery;
  assert.equal(c.state().worktree, 'B'); assert.equal(c.state().diff.diff, 'B:unstaged');
});

test('leaving Changes cancels external staging recovery without reopening a diff', async () => {
  const scan = deferred(); let moved = false, reads = 0, diffs = 0;
  const c = createWorktreeChangesController({
    loadChanges: async () => ++reads > 1 ? scan.promise : { files: [{ path: 'new.py', group: 'unstaged' }] },
    loadDiff: async () => { diffs++; if (moved) throw Object.assign(new Error('file is no longer changed in this group'), { status: 404 }); return { diff: 'original' }; }, onChange() {},
  });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  moved = true; const recovery = c.refreshDiff(); await tick(); c.pause();
  scan.resolve({ files: [{ path: 'new.py', group: 'staged' }] }); await recovery;
  assert.equal(diffs, 2); assert.equal(c.state().loading, false);
});

test('external commit removes a stale file without leaving a group error', async () => {
  let committed = false;
  const c = createWorktreeChangesController({
    loadChanges: async () => ({ files: committed ? [] : [{ path: 'new.py', group: 'staged' }] }),
    loadDiff: async () => { if (committed) throw Object.assign(new Error('file is no longer changed in this group'), { status: 404 }); return { diff: 'original' }; }, onChange() {},
  });
  c.setWorktrees(inventory); await c.selectWorktree('A'); committed = true; await c.refreshDiff();
  assert.deepEqual(c.state().files, []); assert.equal(c.state().selected, ''); assert.equal(c.state().error, '');
});

test('external recovery is bounded and genuine diff failures do not trigger rescans', async () => {
  for (const status of [404, 500]) {
    let broken = false, scans = 0, diffs = 0;
    const c = createWorktreeChangesController({
      loadChanges: async () => { scans++; return { files: [{ path: 'new.py', group: 'unstaged' }] }; },
      loadDiff: async () => { diffs++; if (broken) throw Object.assign(new Error('diff failure'), { status }); return { diff: 'original' }; }, onChange() {},
    });
    c.setWorktrees(inventory); await c.selectWorktree('A'); broken = true; await c.refreshDiff();
    assert.equal(scans, status === 404 ? 2 : 1); assert.equal(diffs, status === 404 ? 3 : 2);
    assert.equal(c.state().error, 'diff failure'); assert.equal(c.state().loading, false);
  }
});

test('display names persist by checkout path without changing Git identity or review state', async () => {
  const prefs = [], reads = [];
  const setup = { loadChanges: async p => { reads.push(p); return { files }; }, loadDiff: async () => ({ diff: 'content' }), onChange() {}, onPreferences: value => prefs.push(value) };
  const checkouts = inventory.map(w => ({ ...w, branch: 'claude/task' }));
  const c = createWorktreeChangesController(setup);
  c.setWorktrees(checkouts); await c.selectWorktree('A'); c.rememberScroll(240);
  c.renameWorktree('A', '  My feature  '); c.hideWorktree('B');
  assert.equal(c.label(checkouts[0]), 'My feature'); assert.equal(c.label(checkouts[1]), 'claude/task');
  assert.equal(checkouts[0].branch, 'claude/task'); assert.equal(checkouts[0].path, 'A');
  assert.equal(c.state().worktree, 'A'); assert.equal(c.state().diff.diff, 'content');
  assert.equal(c.state().positions.get('unstaged:same.ts').top, 240);
  assert.deepEqual(reads, ['A']);
  const restored = createWorktreeChangesController({ ...setup, ...prefs.at(-1) });
  restored.setWorktrees(checkouts);
  assert.equal(restored.label(checkouts[0]), 'My feature'); assert.equal(restored.state().hidden.has('B'), true);
  restored.renameWorktree('A', '');
  assert.equal(restored.label(checkouts[0]), 'claude/task'); assert.deepEqual(prefs.at(-1).names, {});
});

test('display names handle stored preferences safely and retain literal text', () => {
  const c = createWorktreeChangesController({ names: { A: 42, B: '   ', C: '<Review & test>' }, loadChanges() {}, loadDiff() {}, onChange() {} });
  c.setWorktrees(inventory);
  assert.equal(c.label(inventory[2]), '<Review & test>'); assert.equal(c.state().names.size, 1);
  c.renameWorktree('outside', 'foreign'); assert.equal(c.state().names.size, 1);
  c.renameWorktree('A', 'a'.repeat(200)); assert.equal(c.label(inventory[0]).length, 120);
  c.renameWorktree('A', '  '); assert.equal(c.state().names.has('A'), false);
});

test('all worktree headers are available without scanning collapsed worktrees', async () => {
  const reads = [];
  const c = createWorktreeChangesController({ loadChanges: async p => { reads.push(p); return { files }; }, loadDiff: async () => ({ diff: '' }), onChange() {} });
  c.setWorktrees(inventory);
  assert.equal(c.state().inventory.length, 3); assert.equal(reads.length, 0);
  await Promise.all([c.toggleWorktree('A'), c.toggleWorktree('B')]);
  assert.deepEqual(reads.sort(), ['A', 'B']);
  assert.equal(c.state().expanded.size, 2);
  assert.equal(c.state().views.get('A').files.length, 2);
  assert.equal(c.state().views.get('B').files.length, 2);
});

test('hiding and restoring persist view preferences and never read a hidden checkout', async () => {
  const reads = [], prefs = [];
  const c = createWorktreeChangesController({ hidden: ['A'], expanded: ['A'], loadChanges: async p => { reads.push(p); return { files }; }, loadDiff: async () => ({ diff: '' }), onChange() {}, onPreferences: value => prefs.push(value) });
  c.setWorktrees(inventory); await tick(); assert.equal(reads.length, 0);
  c.restoreWorktree('A'); await c.toggleWorktree('A');
  await c.selectFile('A', 'unstaged:same.ts'); c.hideWorktree('A');
  assert.equal(c.state().worktree, ''); assert.equal(c.state().hidden.has('A'), true);
  assert.equal(c.state().expanded.has('A'), false);
  assert.deepEqual(prefs.at(-1).hidden, ['A']);
  const restored = createWorktreeChangesController({ ...prefs.at(-1), loadChanges: async p => { throw new Error('must not scan hidden worktrees'); }, loadDiff() {}, onChange() {} });
  restored.setWorktrees(inventory); await tick(); assert.equal(restored.state().hidden.has('A'), true);
  restored.restoreAll(); assert.equal(restored.state().hidden.size, 0);
});

test('rapid selection cannot let an earlier checkout steal the active diff', async () => {
  const a = deferred();
  const c = createWorktreeChangesController({ loadChanges: p => p === 'A' ? a.promise : Promise.resolve({ files }), loadDiff: async (p, f) => ({ diff: p + ':' + f.group }), onChange() {} });
  c.setWorktrees(inventory);
  const old = c.selectWorktree('A'); await c.selectWorktree('B');
  a.resolve({ files }); await old;
  assert.equal(c.state().worktree, 'B'); assert.equal(c.state().diff.diff, 'B:unstaged');
});

test('warm switches reuse file lists and restore each checkout selection and scroll', async () => {
  const reads = [];
  const c = createWorktreeChangesController({ loadChanges: async p => { reads.push(p); return { files }; }, loadDiff: async (p, f) => ({ diff: p + f.group }), onChange() {} });
  c.setWorktrees(inventory); await c.selectWorktree('A'); await c.selectFile('A', 'staged:same.ts'); c.rememberScroll(320, 12);
  await c.selectWorktree('B'); await c.selectWorktree('A');
  assert.deepEqual(reads, ['A', 'B']); assert.equal(c.state().selected, 'staged:same.ts');
  assert.deepEqual(c.state().positions.get('staged:same.ts'), { top: 320, left: 12 });
});

test('background diff refresh cannot cancel a pending checkout selection', async () => {
  const b = deferred();
  const c = createWorktreeChangesController({ loadChanges: p => p === 'B' ? b.promise : Promise.resolve({ files }), loadDiff: async p => ({ diff: p }), onChange() {} });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  const next = c.selectWorktree('B'); await c.refreshDiff();
  b.resolve({ files }); await next;
  assert.equal(c.state().worktree, 'B'); assert.equal(c.state().diff.diff, 'B');
});

test('expanded checkout scans are bounded to two at a time', async () => {
  let running = 0, peak = 0; const waits = new Map(inventory.map(w => [w.path, deferred()]));
  const c = createWorktreeChangesController({ loadChanges: async p => { running++; peak = Math.max(peak, running); await waits.get(p).promise; running--; return { files }; }, loadDiff: async () => ({ diff: '' }), onChange() {} });
  c.setWorktrees(inventory);
  const jobs = inventory.map(w => c.toggleWorktree(w.path)); await tick();
  assert.equal(running, 2); waits.get('A').resolve(); await tick();
  assert.equal(running, 2); waits.get('B').resolve(); waits.get('C').resolve(); await Promise.all(jobs);
  assert.equal(peak, 2);
});

test('a slow staged diff cannot replace a newer unstaged selection', async () => {
  const staged = deferred();
  const c = createWorktreeChangesController({ loadChanges: async () => ({ files }), loadDiff: async (_, f) => f.group === 'staged' ? staged.promise : { diff: 'unstaged' }, onChange() {} });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  const old = c.selectFile('A', 'staged:same.ts'); await c.selectFile('A', 'unstaged:same.ts');
  staged.resolve({ diff: 'wrong staged content' }); await old; assert.equal(c.state().diff.diff, 'unstaged');
});

test('leaving Changes cancels pending selections without opening another diff', async () => {
  const pending = deferred(); let signal, diffReads = 0;
  const c = createWorktreeChangesController({ loadChanges: (_, s) => { signal = s; return pending.promise; }, loadDiff: async () => { diffReads++; return { diff: '' }; }, onChange() {} });
  c.setWorktrees(inventory); const old = c.selectWorktree('A'); await tick(); c.pause();
  assert.equal(signal.aborted, true); pending.resolve({ files }); await old; assert.equal(diffReads, 0);
});

test('diff rendering escapes source text and does not expose editor action targets', () => {
  const h = [{ oldStart: 1, newStart: 1, section: '<script>bad</script>', rows: [
    { type: 'del', oldLine: 1, text: '<img src=x onerror=bad()>' },
    { type: 'add', newLine: 1, text: 'new text' },
    { type: 'ctx', oldLine: 2, newLine: 2, text: 'context' },
  ] }];
  const split = worktreeDiffHTML(h);
  assert.ok(split.html.includes('&lt;img'));
  assert.ok(split.html.includes('&lt;script&gt;'));
  assert.ok(split.html.includes('diff-row-pair'));
  assert.ok(!split.html.includes('data-l='));
  assert.ok(!split.html.includes('line-btn'));
  const unified = worktreeDiffHTML(h, 'unified', 2);
  assert.equal(unified.remaining, 1);
  assert.ok(unified.html.includes('diff-unified'));
  assert.ok(!unified.html.includes('context'));
});

test('staging merges scoped changes without losing other files or accepting an older scan', async () => {
  const scan = deferred(), mutation = deferred(); let reads = 0;
  const original = [{ path: 'same.ts', group: 'unstaged' }, { path: 'other.ts', group: 'unstaged' }];
  const c = createWorktreeChangesController({ loadChanges: async () => ++reads === 1 ? { files: original } : scan.promise, loadDiff: async (_, f) => ({ diff: f.group }), mutateChanges: async () => mutation.promise, onChange() {} });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  const refresh = c.refreshExpanded(true); const action = c.mutateWorktree('A', 'unstaged', original[0]);
  assert.equal(c.state().views.get('A').mutating, true);
  mutation.resolve({ partial: true, paths: ['same.ts'], files: [{ path: 'same.ts', group: 'staged' }] }); await action;
  scan.resolve({ files: original }); await refresh;
  assert.equal(c.state().selected, 'staged:same.ts'); assert.equal(c.state().diff.diff, 'staged');
  assert.equal(c.state().files.some(f => f.path === 'other.ts'), true); assert.equal(c.state().files.some(f => f.path === 'same.ts' && f.group === 'unstaged'), false);
  assert.equal(c.state().views.get('A').mutating, false);
});

test('staging failures preserve file state and release busy controls', async () => {
  const c = createWorktreeChangesController({ loadChanges: async () => ({ files }), loadDiff: async () => ({ diff: 'original' }), mutateChanges: async () => { throw new Error('index is locked'); }, onChange() {} });
  c.setWorktrees(inventory); await c.selectWorktree('A');
  await assert.rejects(c.mutateWorktree('A', 'unstaged', files[1]), /locked/);
  assert.deepEqual(c.state().files, files); assert.equal(c.state().views.get('A').mutating, false);
});

test('reload restores the selected worktree, file and scroll against fresh Git state', async () => {
  let saved;
  const options = {
    expanded: ['A', 'B'], onChange() {},
    loadChanges: async () => ({ files: [{ path: 'first.py', group: 'unstaged' }, { path: 'second.py', group: 'staged' }] }),
    loadDiff: async (path, file) => ({ diff: path + ':' + file.path }),
  };
  const before = createWorktreeChangesController(options);
  before.setWorktrees(inventory); await before.selectWorktree('B'); await before.selectFile('B', 'staged:second.py');
  before.rememberScroll(143, 21); saved = before.session();
  const after = createWorktreeChangesController(options);
  after.setWorktrees(inventory); await after.restoreSession(saved);
  assert.equal(after.state().worktree, 'B'); assert.equal(after.state().selected, 'staged:second.py');
  assert.equal(after.state().diff.diff, 'B:second.py');
  assert.deepEqual(after.state().positions.get('staged:second.py'), { top: 143, left: 21 });
});

test('reload reconciles moved/deleted files and ignores unavailable worktrees', async () => {
  const saved = { active: 'A', views: [{ path: 'A', file: { path: 'same.py', group: 'unstaged' }, positions: [] }] };
  for (const files of [[{ path: 'same.py', group: 'staged' }], []]) {
    const c = createWorktreeChangesController({ expanded: ['A'], onChange() {}, loadChanges: async () => ({ files }), loadDiff: async () => ({ diff: 'fresh' }) });
    c.setWorktrees(inventory); await c.restoreSession(saved);
    assert.equal(c.state().selected, files.length ? 'staged:same.py' : ''); assert.equal(c.state().error, '');
  }
  let reads = 0;
  const c = createWorktreeChangesController({ onChange() {}, loadChanges: async () => { reads++; return { files: [] }; }, loadDiff: async () => { reads++; } });
  c.setWorktrees(inventory); await c.restoreSession({ ...saved, active: 'removed', views: [{ path: 'removed', file: saved.views[0].file }] });
  assert.equal(c.state().worktree, ''); assert.equal(reads, 0);
});

test('a user selection made during reload recovery wins over saved state', async () => {
  const pending = deferred();
  const c = createWorktreeChangesController({ expanded: ['A'], onChange() {}, loadChanges: async path => path === 'A' ? pending.promise : { files: [{ path: 'b.py', group: 'unstaged' }] }, loadDiff: async path => ({ diff: path }) });
  c.setWorktrees(inventory);
  const restoring = c.restoreSession({ active: 'A', views: [{ path: 'A', file: { path: 'a.py', group: 'unstaged' } }] });
  await c.selectWorktree('B'); pending.resolve({ files: [{ path: 'a.py', group: 'unstaged' }] }); await restoring;
  assert.equal(c.state().worktree, 'B'); assert.equal(c.state().diff.diff, 'B');
});
