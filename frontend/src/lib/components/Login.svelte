<script>
  import { login } from '../api.js';

  let { onAuthenticated } = $props();

  let token = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e) {
    e.preventDefault();
    if (!token.trim() || busy) return;
    busy = true;
    try {
      await login(token.trim());
      error = '';
      token = '';
      onAuthenticated();
    } catch (err) {
      error = err.message;
    }
    busy = false;
  }
</script>

<div class="wrap">
  <form class="panel" onsubmit={submit}>
    <div class="head">
      <h1>fanout<span>d</span></h1>
      <p>This board needs its access token.</p>
    </div>
    <div class="body">
      <label class="field">
        <span class="eyebrow">Access token</span>
        <input
          type="password"
          bind:value={token}
          autocomplete="current-password"
          disabled={busy}
        />
      </label>
      {#if error}
        <div class="notice bad">{error}</div>
      {/if}
      <button class="btn primary wide" type="submit" disabled={busy || !token.trim()}>
        {busy ? 'Checking…' : 'Unlock'}
      </button>
    </div>
  </form>
</div>

<style>
  .wrap {
    display: flex;
    justify-content: center;
    align-items: center;
    min-height: 100vh;
    padding: 20px;
  }
  form { width: 100%; max-width: 340px; }
  .head { padding: 16px 20px; border-bottom: 1px solid var(--rule); }
  h1 {
    margin: 0;
    font-family: var(--f-mono);
    font-size: 16px;
    font-weight: 400;
    letter-spacing: .16em;
  }
  h1 span { color: var(--ink-3); }
  .head p {
    margin: 8px 0 0;
    font-family: var(--f-display);
    font-style: italic;
    font-size: 16px;
    color: var(--ink-2);
  }
  .body { padding: 18px 20px 20px; }
  .notice { margin-bottom: 12px; }
  .wide { width: 100%; padding: 9px 12px; }
</style>
