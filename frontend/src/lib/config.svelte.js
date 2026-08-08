// The server settings that change what the board means rather than how it looks.
//
// Only one thing needs them, and it is the thing the tasks cannot say: an empty
// Review column is either nothing waiting or nothing that will ever be filed
// there, and those want opposite words. Fetched once when the board opens and
// again whenever the settings page saves, which is the only other way they move
// under a running server.

import { fetchConfig } from './api.js';

export const settings = $state({
  review: false,
  reviewModel: '',
  shell: false,
  loaded: false,
});

export async function loadSettings() {
  try {
    const cfg = await fetchConfig();
    settings.review = !!cfg.review;
    settings.reviewModel = cfg.review_model ?? '';
    settings.shell = !!cfg.shell;
  } catch (e) {
    // An unreachable settings endpoint is not worth a banner: the board reports
    // that for itself, and the defaults above only make the UI quieter.
    console.error('Failed to load settings', e);
  }
  settings.loaded = true;
}
