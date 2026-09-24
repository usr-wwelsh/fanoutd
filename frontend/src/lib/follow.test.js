import { describe, expect, test } from 'bun:test';
import { nearBottom } from './follow.js';

const box = (scrollTop, scrollHeight = 2000, clientHeight = 800) => ({ scrollTop, scrollHeight, clientHeight });

describe('nearBottom', () => {
  test('at the bottom is following', () => {
    expect(nearBottom(box(1200))).toBe(true);
  });

  test('a few pixels short still counts, so sub-pixel scroll does not drop the follow', () => {
    expect(nearBottom(box(1170))).toBe(true);
  });

  test('scrolled up to read is not following', () => {
    expect(nearBottom(box(600))).toBe(false);
  });

  test('content shorter than the box is always at the bottom', () => {
    expect(nearBottom(box(0, 500))).toBe(true);
  });

  test('nothing to scroll is not following', () => {
    expect(nearBottom(null)).toBe(false);
  });
});
