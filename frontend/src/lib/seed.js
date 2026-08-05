// Seeding from the browser is the same idea as the CLI's --seed: files are read
// on this side and their contents sent, because the server may be on another
// machine and a local path means nothing there. The picker gives us the two
// shapes the CLI has — a file keeps its own name, a folder keeps its name as a
// prefix — so `assets/` picked here arrives exactly as `--seed assets/` does.

// The server enforces these too. They live here so a bad pick is refused with
// the offending file named, before a slow breakdown request is sent.
export const MAX_SEED_FILE_BYTES = 256 * 1024;
export const MAX_SEED_TOTAL_BYTES = 2 * 1024 * 1024;
export const MAX_SEED_FILES = 200;

// seedPath is where a picked file lands in the workspace. A folder pick carries
// webkitRelativePath, which already starts at the folder the user chose.
export function seedPath(file) {
  const raw = file.webkitRelativePath || file.name || '';
  return raw.replace(/\\/g, '/').replace(/^\.\//, '');
}

// collectSeed reads a pick into the seed the API takes, merging onto `existing`
// so a second pick adds rather than replaces. Files that cannot be seeded are
// skipped with a reason rather than failing the pick — a chosen folder holding
// one image should still seed its text. What no per-file skip can fix (too many
// files, over the total) throws, leaving the previous seed untouched.
export async function collectSeed(picked, existing = []) {
  const files = [...existing];
  const skipped = [];
  const seen = new Set(files.map(f => f.path));
  let total = files.reduce((n, f) => n + byteLength(f.content), 0);

  for (const file of picked) {
    const path = seedPath(file);
    if (!path || hasDottedPart(path) || path.split('/').includes('..')) {
      if (path) skipped.push({ path, reason: 'dotted path' });
      continue;
    }
    if (seen.has(path)) continue;
    if (file.size > MAX_SEED_FILE_BYTES) {
      skipped.push({ path, reason: `over the ${MAX_SEED_FILE_BYTES} byte file limit` });
      continue;
    }

    const content = await file.text();
    if (!isText(content)) {
      skipped.push({ path, reason: 'not text' });
      continue;
    }
    const size = byteLength(content);
    if (size > MAX_SEED_FILE_BYTES) {
      skipped.push({ path, reason: `over the ${MAX_SEED_FILE_BYTES} byte file limit` });
      continue;
    }

    if (files.length + 1 > MAX_SEED_FILES) {
      throw new Error(`a seed holds at most ${MAX_SEED_FILES} files; pick a narrower folder`);
    }
    total += size;
    if (total > MAX_SEED_TOTAL_BYTES) {
      throw new Error(`that is over the ${MAX_SEED_TOTAL_BYTES} byte seed total; pick a narrower folder`);
    }

    seen.add(path);
    files.push({ path, content });
  }

  files.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return { files, skipped };
}

// describeSeed is the line under the picker, so it is clear what the agent will
// start with.
export function describeSeed(files) {
  if (!files.length) return '';
  const bytes = files.reduce((n, f) => n + byteLength(f.content), 0);
  return `${files.length} file${files.length === 1 ? '' : 's'}, ${bytes} bytes`;
}

// A dotted segment anywhere is skipped, the way the CLI skips them at every
// level of a walk: .git and .env are the two things in a working directory that
// must not be handed to an agent.
function hasDottedPart(path) {
  return path.split('/').some(part => part.startsWith('.'));
}

// text() decodes anything as UTF-8, so binary comes back as replacement
// characters rather than an error. A NUL or a U+FFFD is what that looks like.
function isText(content) {
  return !content.includes('\u0000') && !content.includes('\ufffd');
}

function byteLength(content) {
  return new TextEncoder().encode(content).length;
}
