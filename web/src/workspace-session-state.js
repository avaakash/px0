// Persist view metadata only: never shell tokens, output, commands or file contents.
export function createWorkspaceSessionStore(storage, root) {
  const key = 'px0.workspaceSession.v1:' + root;
  return {
    read() {
      try {
        const raw = storage.getItem(key);
        if (!raw || raw.length > 262144) return null;
        const value = JSON.parse(raw);
        return value?.version === 1 && value.root === root ? value : null;
      } catch { return null; }
    },
    write(value) {
      try {
        const raw = JSON.stringify({ ...value, version: 1, root });
        if (raw.length <= 262144) storage.setItem(key, raw);
      } catch {} // Storage can be disabled or full without breaking the workspace.
    },
  };
}

export function terminalRestoreSelection(sessions, saved) {
  const ids = new Set(sessions.map(s => s.id));
  return { active: ids.has(saved?.active) ? saved.active : sessions[0]?.id || '', visible: saved?.visible === true && sessions.length > 0 };
}

export const sessionNumber = (value, min, max, fallback = min) => typeof value === 'number' && Number.isFinite(value) ? Math.max(min, Math.min(max, value)) : fallback;
