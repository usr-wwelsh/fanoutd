export const DEFAULT_WIDTH = 480;
const MIN_WIDTH = 360;
const MAX_SHARE = 0.92;
const KEY = 'fanoutd.panelWidth';

export function clampWidth(width, viewport) {
  const max = Math.floor(viewport * MAX_SHARE);
  return Math.min(Math.max(width, MIN_WIDTH), max);
}

export function readWidth(storage = localStorage) {
  try {
    const n = Number.parseInt(storage.getItem(KEY) ?? '', 10);
    return Number.isFinite(n) ? n : DEFAULT_WIDTH;
  } catch {
    return DEFAULT_WIDTH;
  }
}

export function saveWidth(width, storage = localStorage) {
  try {
    storage.setItem(KEY, String(width));
  } catch {
    // A width that is not remembered is the default next time, which is fine.
  }
}
