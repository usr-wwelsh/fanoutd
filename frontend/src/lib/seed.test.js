import { describe, expect, test } from 'bun:test';
import { MAX_SEED_FILE_BYTES, MAX_SEED_FILES, collectSeed, describeSeed, seedPath } from './seed.js';

// A picked file stands in for a browser File: a name, an optional directory
// path from a folder pick, and text().
function pick(name, content = 'x', relative = '') {
  const file = new File([content], name.split('/').pop(), { type: 'text/plain' });
  if (relative) Object.defineProperty(file, 'webkitRelativePath', { value: relative });
  return file;
}

describe('seedPath', () => {
  test('a single file keeps its own name', () => {
    expect(seedPath(pick('spec.md'))).toBe('spec.md');
  });

  test('a folder pick keeps the folder as a prefix', () => {
    expect(seedPath(pick('main.css', '', 'assets/css/main.css'))).toBe('assets/css/main.css');
  });

  test('a leading ./ is dropped', () => {
    expect(seedPath(pick('a.md', '', './docs/a.md'))).toBe('docs/a.md');
  });
});

describe('collectSeed', () => {
  test('reads picked files into path and content', async () => {
    const { files } = await collectSeed([pick('spec.md', '# spec')]);
    expect(files).toEqual([{ path: 'spec.md', content: '# spec' }]);
  });

  test('sorts by path so the same pick makes the same seed', async () => {
    const { files } = await collectSeed([
      pick('b.md', 'b', 'docs/b.md'),
      pick('a.md', 'a', 'docs/a.md'),
    ]);
    expect(files.map(f => f.path)).toEqual(['docs/a.md', 'docs/b.md']);
  });

  test('skips dotted names at every level, so .git and .env never travel', async () => {
    const { files, skipped } = await collectSeed([
      pick('README.md', 'r', 'repo/README.md'),
      pick('.env', 'SECRET=1', 'repo/.env'),
      pick('HEAD', 'ref', 'repo/.git/HEAD'),
    ]);
    expect(files.map(f => f.path)).toEqual(['repo/README.md']);
    expect(skipped.map(s => s.path)).toEqual(['repo/.env', 'repo/.git/HEAD']);
  });

  test('skips binary content rather than mangling it', async () => {
    const png = new File([new Uint8Array([0x89, 0x50, 0x00, 0x1a])], 'logo.png');
    const { files, skipped } = await collectSeed([pick('a.md', 'a'), png]);
    expect(files.map(f => f.path)).toEqual(['a.md']);
    expect(skipped[0]).toEqual({ path: 'logo.png', reason: 'not text' });
  });

  test('skips a file over the per-file limit and names it', async () => {
    const big = pick('big.md', 'x'.repeat(MAX_SEED_FILE_BYTES + 1));
    const { files, skipped } = await collectSeed([big]);
    expect(files).toEqual([]);
    expect(skipped[0].reason).toContain('over');
  });

  test('drops a path picked twice', async () => {
    const { files } = await collectSeed([pick('a.md', 'first'), pick('a.md', 'second')]);
    expect(files).toEqual([{ path: 'a.md', content: 'first' }]);
  });

  test('rejects the whole pick when it is over the total, so nothing is half-seeded', async () => {
    const half = 'x'.repeat(MAX_SEED_FILE_BYTES);
    const many = Array.from({ length: 9 }, (_, i) => pick(`f${i}.md`, half));
    await expect(collectSeed(many)).rejects.toThrow(/total/);
  });

  test('rejects more files than the limit', async () => {
    const many = Array.from({ length: MAX_SEED_FILES + 1 }, (_, i) => pick(`f${i}.md`, 'x'));
    await expect(collectSeed(many)).rejects.toThrow(/files/);
  });

  test('merges into an existing seed so a second pick adds to the first', async () => {
    const { files: first } = await collectSeed([pick('a.md', 'a')]);
    const { files } = await collectSeed([pick('b.md', 'b')], first);
    expect(files.map(f => f.path)).toEqual(['a.md', 'b.md']);
  });
});

describe('describeSeed', () => {
  test('counts files and bytes', () => {
    expect(describeSeed([{ path: 'a', content: 'xx' }])).toBe('1 file, 2 bytes');
    expect(describeSeed([{ path: 'a', content: 'xx' }, { path: 'b', content: 'y' }]))
      .toBe('2 files, 3 bytes');
  });

  test('is empty for an empty seed', () => {
    expect(describeSeed([])).toBe('');
  });
});
