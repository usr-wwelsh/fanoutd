import { describe, expect, test } from 'bun:test';
import { DEFAULT_WIDTH, clampWidth, readWidth } from './panel.js';

describe('clampWidth', () => {
  test('keeps a width that fits', () => {
    expect(clampWidth(700, 1600)).toBe(700);
  });

  test('never narrows past what the detail needs to stay readable', () => {
    expect(clampWidth(100, 1600)).toBe(360);
  });

  test('never covers the whole board', () => {
    expect(clampWidth(5000, 1000)).toBe(920);
  });

  test('on a screen narrower than the minimum, the screen wins', () => {
    expect(clampWidth(480, 350)).toBe(322);
  });
});

describe('readWidth', () => {
  const storage = (value) => ({ getItem: () => value });

  test('reads a saved width back', () => {
    expect(readWidth(storage('640'))).toBe(640);
  });

  test('falls back to the default when nothing is saved', () => {
    expect(readWidth(storage(null))).toBe(DEFAULT_WIDTH);
  });

  test('falls back to the default on garbage', () => {
    expect(readWidth(storage('wide'))).toBe(DEFAULT_WIDTH);
  });

  test('falls back to the default when storage refuses to be read', () => {
    expect(readWidth({ getItem: () => { throw new Error('blocked'); } })).toBe(DEFAULT_WIDTH);
  });
});
