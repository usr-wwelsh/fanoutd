// The theme is the system's until you say otherwise, and it goes back to the
// system on the next open. Nothing is written to storage, so there is never a
// stale preference to load and un-paint — the CSS resolves the default itself,
// before the first frame, and this module only carries an override.

const query = window.matchMedia('(prefers-color-scheme: dark)');

export const theme = $state({
  system: query.matches ? 'dark' : 'light',
  override: null,
});

// Following the system means following it while the page is open too, not just
// at load — flipping the OS switch repaints without a reload.
query.addEventListener('change', (e) => {
  theme.system = e.matches ? 'dark' : 'light';
});

export function active() {
  return theme.override ?? theme.system;
}

// One control, so it toggles: away from the system, then back to it once the
// two agree again rather than pinning a choice that looks like no choice.
export function toggleTheme() {
  const next = active() === 'dark' ? 'light' : 'dark';
  theme.override = next === theme.system ? null : next;
  if (theme.override) document.documentElement.dataset.theme = theme.override;
  else delete document.documentElement.dataset.theme;
}
