import { describe, expect, test } from 'bun:test';
import { settled, tabTitle } from './attention.js';

describe('tabTitle', () => {
  test('is just the name when nothing needs attention', () => {
    expect(tabTitle({ running: 0, held: 0 })).toBe('fanoutd');
  });

  test('leads with what is running and what waits on review', () => {
    expect(tabTitle({ running: 2, held: 1 })).toBe('(2 running · 1 review) fanoutd');
  });

  test('omits a count that is zero', () => {
    expect(tabTitle({ running: 0, held: 3 })).toBe('(3 review) fanoutd');
  });
});

const task = (id, status, title = id) => ({ id, status, title });

describe('settled', () => {
  test('reports a run that finished since the last poll', () => {
    const prev = [task('a', 'running')];
    const next = [task('a', 'done')];
    expect(settled(prev, next)).toEqual([next[0]]);
  });

  test('reports a run that failed', () => {
    expect(settled([task('a', 'running')], [task('a', 'error')])).toHaveLength(1);
  });

  test('stays quiet about a run someone stopped', () => {
    expect(settled([task('a', 'running')], [task('a', 'stopped')])).toEqual([]);
  });

  test('stays quiet on the first load, when there is nothing to compare against', () => {
    expect(settled([], [task('a', 'done')])).toEqual([]);
  });

  test('stays quiet about work that was already finished', () => {
    expect(settled([task('a', 'done')], [task('a', 'done')])).toEqual([]);
  });
});
