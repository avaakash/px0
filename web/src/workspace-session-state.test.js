import test from 'node:test';
import assert from 'node:assert/strict';
import { createWorkspaceSessionStore, terminalRestoreSelection, sessionNumber } from './workspace-session-state.js';

test('reload state is isolated per workspace and survives a new store instance', () => {
  const values = new Map(), storage = { getItem: k => values.get(k), setItem: (k, v) => values.set(k, v) };
  const first = createWorkspaceSessionStore(storage, '/repo/one');
  first.write({ mode: 'worktrees', terminal: { active: 'shell-two', visible: true } });
  assert.equal(createWorkspaceSessionStore(storage, '/repo/one').read().terminal.active, 'shell-two');
  assert.equal(createWorkspaceSessionStore(storage, '/repo/two').read(), null);
  assert.equal(values.size, 1);
});

test('bad, oversized and unavailable browser storage cannot break startup', () => {
  for (const raw of ['{', 'null', '[]', '{"version":2,"root":"A"}', '{"version":1,"root":"B"}', ' '.repeat(262145)]) {
    assert.equal(createWorkspaceSessionStore({ getItem: () => raw }, 'A').read(), null);
  }
  const blocked = createWorkspaceSessionStore({ getItem() { throw Error(); }, setItem() { throw Error(); } }, 'A');
  assert.equal(blocked.read(), null); assert.doesNotThrow(() => blocked.write({ mode: 'code' }));
  assert.doesNotThrow(() => createWorkspaceSessionStore(undefined, 'A').write({}));
});

test('terminal restoration selects a surviving shell, preserves hidden state and never invents a session', () => {
  const sessions = [{ id: 'first' }, { id: 'second' }];
  assert.deepEqual(terminalRestoreSelection(sessions, { active: 'second', visible: true }), { active: 'second', visible: true });
  assert.deepEqual(terminalRestoreSelection(sessions, { active: 'second', visible: false }), { active: 'second', visible: false });
  assert.deepEqual(terminalRestoreSelection(sessions, { active: 'expired', visible: true }), { active: 'first', visible: true });
  assert.deepEqual(terminalRestoreSelection([], { active: 'second', visible: true }), { active: '', visible: false });
});

test('saved layout numbers are bounded and reject non-finite values', () => {
  assert.equal(sessionNumber(Infinity, 0, 300), 0);
  assert.equal(sessionNumber('200', 0, 300), 0);
  assert.equal(sessionNumber(-2, 0, 300), 0);
  assert.equal(sessionNumber(500, 0, 300), 300);
});
