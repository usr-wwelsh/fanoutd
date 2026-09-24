import { describe, expect, test } from 'bun:test';
import { pendingDeletes, visible } from './pending.js';

function clock() {
  const timers = new Map();
  let next = 0;
  return {
    setTimer: (fn) => { timers.set(++next, fn); return next; },
    clearTimer: (id) => timers.delete(id),
    fire: () => { for (const [id, fn] of [...timers]) { timers.delete(id); fn(); } },
    count: () => timers.size,
  };
}

function setup() {
  const c = clock();
  const committed = [];
  const changes = [];
  const q = pendingDeletes({
    commit: (entry) => committed.push(entry.id),
    onChange: (list) => changes.push(list),
    setTimer: c.setTimer,
    clearTimer: c.clearTimer,
  });
  return { c, q, committed, changes };
}

describe('pendingDeletes', () => {
  test('deletes once the undo window runs out', () => {
    const { c, q, committed } = setup();
    q.add({ kind: 'task', id: 'a' });
    expect(committed).toEqual([]);
    c.fire();
    expect(committed).toEqual(['a']);
  });

  test('undo within the window means nothing is deleted', () => {
    const { c, q, committed } = setup();
    q.add({ kind: 'task', id: 'a' });
    q.undo('a');
    c.fire();
    expect(committed).toEqual([]);
  });

  test('flush deletes everything still waiting, now', () => {
    const { c, q, committed } = setup();
    q.add({ kind: 'task', id: 'a' });
    q.add({ kind: 'group', id: 'g' });
    q.flush();
    expect(committed).toEqual(['a', 'g']);
    expect(c.count()).toBe(0);
  });

  test('reports what is waiting whenever that changes', () => {
    const { c, q, changes } = setup();
    q.add({ kind: 'task', id: 'a' });
    c.fire();
    expect(changes.map(l => l.map(e => e.id))).toEqual([['a'], []]);
  });

  test('adding the same thing twice keeps one timer', () => {
    const { c, q } = setup();
    q.add({ kind: 'task', id: 'a' });
    q.add({ kind: 'task', id: 'a' });
    expect(c.count()).toBe(1);
  });
});

describe('visible', () => {
  const tasks = [
    { id: 'a' },
    { id: 'b', group_id: 'g' },
    { id: 'c', group_id: 'g' },
    { id: 'd' },
  ];

  test('hides a task waiting to be deleted', () => {
    expect(visible(tasks, [{ kind: 'task', id: 'a' }]).map(t => t.id)).toEqual(['b', 'c', 'd']);
  });

  test('hides every subtask of a plan waiting to be deleted', () => {
    expect(visible(tasks, [{ kind: 'group', id: 'g' }]).map(t => t.id)).toEqual(['a', 'd']);
  });

  test('returns the same list when nothing is waiting', () => {
    expect(visible(tasks, [])).toBe(tasks);
  });
});
