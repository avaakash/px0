import test from 'node:test';
import assert from 'node:assert/strict';
import { createWorktreePRController, reviewLabel, checksLabel, safeCheckURL, worktreePRNotice } from './worktree-pr-state.js';
const tick = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };

test('PR status queue is bounded, cached, and independent for each worktree', async () => {
  const waits = new Map(['A', 'B', 'C'].map(path => [path, deferred()]));
  let running = 0, peak = 0, reads = 0, time = 0;
  const c = createWorktreePRController({ now: () => time, onChange() {}, load: async path => { reads++; running++; peak = Math.max(peak, running); await waits.get(path).promise; running--; return { number: path }; } });
  c.sync(['A', 'B', 'C'], true); await tick(); assert.equal(reads, 2);
  waits.get('A').resolve(); await tick(); assert.equal(reads, 3);
  waits.get('B').resolve(); waits.get('C').resolve(); await tick();
  c.refreshAll(); await tick(); assert.equal(reads, 3); assert.equal(peak, 2);
  assert.equal(c.entries.get('C').value.number, 'C');
  time = 120001; c.refreshAll(); await tick(); await tick(); assert.equal(reads, 6);
});

test('hiding worktrees or the page cancels reads and rejects their late results', async () => {
  const waiting = deferred(); let reads = 0, signal;
  const c = createWorktreePRController({ onChange() {}, load: (path, force, s) => { reads++; signal = s; return waiting.promise; } });
  c.sync(['A', 'B'], true); await tick();
  c.sync([], false); assert.equal(signal.aborted, true); assert.equal(c.entries.size, 0);
  waiting.resolve({ number: 7 }); await tick(); assert.equal(c.entries.size, 0);
  c.refreshAll(); await tick(); assert.equal(reads, 2);
});

test('a failed refresh preserves the last result and reports it as stale', async () => {
  let fail = false;
  const c = createWorktreePRController({ onChange() {}, load: async () => { if (fail) throw new Error('GitHub unavailable'); return { number: 7 }; } });
  c.sync(['A'], true); await tick(); fail = true; c.refresh('A', true); await tick();
  assert.equal(c.entries.get('A').value.number, 7); assert.equal(c.entries.get('A').error, 'GitHub unavailable'); assert.equal(c.entries.get('A').loading, false);
});

test('review labels and check summaries do not call skipped or missing checks passed', () => {
  assert.equal(reviewLabel({ reviewState: 'requested' }), 'Reviewer requested');
  assert.equal(reviewLabel({ reviewState: 'reviewed' }), 'Review received');
  assert.equal(reviewLabel({ reviewState: 'approved', reviewRequirements: 'satisfied' }), 'Approval received');
  assert.equal(checksLabel({ checksState: 'none', checks: [] }), 'No checks');
  assert.equal(checksLabel({ checksState: 'passed', checks: [{ state: 'skipped' }] }), 'Checks skipped');
  assert.equal(checksLabel({ checksState: 'failed', checks: [{ state: 'passed' }, { state: 'failed' }] }), '1 failed check');
});

test('remote check links reject scripts and embedded credentials', () => {
  assert.equal(safeCheckURL('javascript:alert(1)'), '');
  assert.equal(safeCheckURL('https://token@github.com/o/r'), '');
  assert.equal(safeCheckURL('https://github.com/o/r/actions/runs/1'), 'https://github.com/o/r/actions/runs/1');
});

test('no PR is a normal empty result; failures never claim that no PR exists', async () => {
  let fail = false;
  const c = createWorktreePRController({ onChange() {}, load: async () => {
    if (fail) throw new Error('This worktree is no longer available. Refresh the worktree list.');
    return { state: 'none', connected: true };
  } });
  c.sync(['A'], true); await tick();
  assert.deepEqual(worktreePRNotice(c.entries.get('A')), { error: '', message: 'No open PR has been raised for this branch.' });
  fail = true; c.refresh('A', true); await tick();
  assert.deepEqual(worktreePRNotice(c.entries.get('A')), { error: 'This worktree is no longer available. Refresh the worktree list.', message: '' });
  assert.deepEqual(worktreePRNotice({ error: 'GitHub unavailable' }), { error: 'GitHub unavailable', message: '' });
  assert.equal(worktreePRNotice({ loading: true }).message, 'Checking GitHub…');
  assert.equal(worktreePRNotice().message, 'PR status has not been checked yet.');
});

test('approval label requires a satisfied GitHub review gate', () => {
  for (const pr of [
    { reviewState: 'approved' },
    { reviewState: 'approved', reviewRequirements: 'required' },
    { reviewState: 'approved', reviewRequirements: 'unknown' },
    { reviewState: 'approved', reviewRequirements: 'changes_requested' },
  ]) assert.equal(reviewLabel(pr), 'Review received');
  assert.equal(reviewLabel({ reviewState: 'approved', reviewRequirements: 'satisfied' }), 'Approval received');
});

test('a failed refresh never keeps the green approval label', () => {
  const pr = { reviewState: 'approved', reviewRequirements: 'satisfied' };
  assert.equal(reviewLabel(pr), 'Approval received');
  assert.equal(reviewLabel(pr, true), 'Review status unavailable');
});
