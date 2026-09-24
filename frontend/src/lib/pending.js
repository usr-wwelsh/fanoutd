// A delete is held for a few seconds before it is sent, so undo is a real undo
// rather than a confirm dialog in front of every one. Closing the page flushes
// what is held: the delete was asked for, and a task that comes back on the
// next load would be the surprise.

export function pendingDeletes({ commit, onChange, delay = 5000, setTimer = setTimeout, clearTimer = clearTimeout }) {
  const held = new Map();
  const changed = () => onChange([...held.values()].map(h => h.entry));

  function release(id) {
    const h = held.get(id);
    if (!h) return null;
    clearTimer(h.timer);
    held.delete(id);
    changed();
    return h.entry;
  }

  return {
    add(entry) {
      if (held.has(entry.id)) return;
      const timer = setTimer(() => { const e = release(entry.id); if (e) commit(e); }, delay);
      held.set(entry.id, { entry, timer });
      changed();
    },
    undo(id) {
      release(id);
    },
    flush() {
      for (const id of [...held.keys()]) commit(release(id));
    },
  };
}

export function visible(tasks, entries) {
  if (!entries.length) return tasks;
  const ids = new Set(entries.filter(e => e.kind === 'task').map(e => e.id));
  const groups = new Set(entries.filter(e => e.kind === 'group').map(e => e.id));
  return tasks.filter(t => !ids.has(t.id) && !groups.has(t.group_id));
}
