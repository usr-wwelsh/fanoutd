<script>
  import { createEventDispatcher } from 'svelte';
  import { fetchSettings, saveSettings } from '../api.js';
  import { loadSettings } from '../config.svelte.js';
  import { changedValues, formFrom, groupFields } from '../settings.js';

  let dispatch = createEventDispatcher();

  let fields = $state([]);
  let form = $state({});
  // The secrets the operator typed into or explicitly cleared. Nothing else can
  // tell an empty box meaning "clear the key" from one meaning "the server
  // never showed me this, and I did not touch it".
  let touched = $state(new Set());
  let file = $state('');
  let restartPending = $state([]);
  let warnings = $state([]);
  let loading = $state(true);
  let saving = $state(false);
  let error = $state('');
  let saved = $state(false);
  let signedOut = $state(false);

  let groups = $derived(groupFields(fields));
  let dirty = $derived(Object.keys(changedValues(fields, form, touched)).length > 0);

  $effect(() => {
    let cancelled = false;
    fetchSettings()
      .then(res => { if (!cancelled) adopt(res); })
      .catch(e => { if (!cancelled) error = e.message; })
      .finally(() => { if (!cancelled) loading = false; });
    return () => { cancelled = true; };
  });

  // adopt resets the form to what the server now says, which after a save is
  // what was actually written — an exported variable can outrule a saved value,
  // and the form has to show that rather than what was typed.
  function adopt(res) {
    fields = res.fields ?? [];
    form = formFrom(fields);
    touched = new Set();
    file = res.file ?? '';
    restartPending = res.restart_pending ?? [];
    warnings = res.warnings ?? [];
  }

  async function handleSave() {
    const values = changedValues(fields, form, touched);
    if (!Object.keys(values).length) {
      dispatch('close');
      return;
    }
    saving = true;
    error = '';
    saved = false;
    const tokenChanged = 'FANOUT_TOKEN' in values;
    try {
      adopt(await saveSettings(values));
      saved = true;
      signedOut = tokenChanged;
      // The board reads review and shell from its own endpoint, so it has to be
      // told they moved rather than waiting for the next reload.
      loadSettings();
      dispatch('saved');
    } catch (e) {
      error = e.message || 'The settings could not be saved.';
      console.error(e);
    }
    saving = false;
  }

  function clearSecret(key) {
    form[key] = '';
    touched = new Set(touched).add(key);
  }

  function markTouched(key) {
    if (!touched.has(key)) touched = new Set(touched).add(key);
  }

  function fieldLabel(f) {
    return restartPending.includes(f.key) ? `${f.label} · saved, waiting for a restart` : f.label;
  }
</script>

<div class="modal-backdrop">
  <div class="modal settings" role="dialog" aria-modal="true" aria-label="Settings">
    <div class="modal-head">
      <div class="eyebrow">{file ? `Written to ${file}` : 'Server settings'}</div>
      <h2>Settings</h2>
    </div>

    {#if loading}
      <div class="modal-body eyebrow">Loading settings…</div>
    {:else}
      <form class="modal-body" onsubmit={(e) => { e.preventDefault(); handleSave(); }}>
        {#if error}
          <div class="notice bad">{error}</div>
        {/if}

        {#each warnings as warning}
          <div class="notice bad">{warning}</div>
        {/each}

        {#if restartPending.length}
          <div class="notice judge">
            <strong>Restart to finish.</strong>
            {restartPending.join(', ')}
            {restartPending.length === 1 ? 'is' : 'are'} saved, but the listener is already bound and
            the database and workspaces are already open. Everything else on this page is in force now.
          </div>
        {/if}

        {#if signedOut}
          <div class="notice judge">
            The access token changed. Every browser holding the old one is signed out, including this
            one — the board will ask for the new token on its next request.
          </div>
        {:else if saved}
          <div class="notice">Saved.</div>
        {/if}

        {#each groups as group (group.name)}
          <section>
            <h3 class="eyebrow group">{group.name}</h3>

            <div class="fields">
            {#each group.fields as f (f.key)}
              {#if f.kind === 'bool'}
                <label class="switch">
                  <input type="checkbox" bind:checked={form[f.key]} disabled={saving} />
                  <span>
                    <span class="switch-label">{fieldLabel(f)}</span>
                    {#if f.help}<span class="hint">{f.help}</span>{/if}
                  </span>
                </label>
              {:else}
                <label class="field" class:half={f.half}>
                  <span class="eyebrow">{fieldLabel(f)}</span>

                  {#if f.kind === 'enum'}
                    <select bind:value={form[f.key]} disabled={saving}>
                      <option value="">Default{f.placeholder ? ` (${f.placeholder})` : ''}</option>
                      {#each f.choices ?? [] as choice}
                        <option value={choice}>{choice}</option>
                      {/each}
                    </select>
                  {:else if f.kind === 'secret'}
                    <!-- The value is never served, so a set secret shows as
                         filled rather than as an empty box: an empty box for a
                         key that is working reads as a key that is missing. -->
                    <span class="secret">
                      <input
                        type="password"
                        autocomplete="new-password"
                        class:filled={f.set && !touched.has(f.key)}
                        bind:value={form[f.key]}
                        oninput={() => markTouched(f.key)}
                        disabled={saving}
                        placeholder={f.set ? `••••••••••••${f.hint ? `  ${f.hint}` : ''}` : 'not set'}
                      />
                      {#if f.set && !touched.has(f.key)}
                        <button class="btn tiny" type="button" onclick={() => clearSecret(f.key)} disabled={saving}>
                          Clear
                        </button>
                      {/if}
                    </span>
                    {#if f.set && !touched.has(f.key)}
                      <span class="hint">In use — not readable from here. Type to replace it.</span>
                    {/if}
                  {:else if f.kind === 'int'}
                    <input type="number" min="0" bind:value={form[f.key]} disabled={saving} placeholder={f.placeholder} />
                  {:else}
                    <input
                      type="text"
                      class:mono={f.kind === 'paths'}
                      bind:value={form[f.key]}
                      disabled={saving}
                      placeholder={f.placeholder}
                    />
                  {/if}

                  <!-- Advice about whether to set a secret is only useful while
                       deciding to; once one is in use it is a third line of
                       small print under a field nobody is reading. -->
                  {#if f.help && !(f.kind === 'secret' && f.set && !touched.has(f.key))}
                    <span class="hint">{f.help}</span>
                  {/if}

                  <!-- A value arriving under a name this setting replaced is
                       still the value in force. Saying which name is what stops
                       it reading as unset. -->
                  {#if f.legacy_key}
                    <span class="hint warn">
                      In force as <span class="mono">{f.legacy_key}</span>. Saving here writes
                      <span class="mono">{f.key}</span> and retires the old name.
                    </span>
                  {/if}

                  {#if f.source === 'env'}
                    <span class="hint warn">
                      Set by an environment variable, which outrules the file both now and at the next
                      start. Unset it in the environment for this field to take.
                    </span>
                  {:else if f.restart && !restartPending.includes(f.key)}
                    <span class="hint">Takes effect at the next restart.</span>
                  {/if}
                </label>
              {/if}
            {/each}
            </div>
          </section>
        {/each}

        <div class="modal-actions">
          <button class="btn" type="button" onclick={() => dispatch('close')} disabled={saving}>
            {dirty ? 'Discard' : 'Close'}
          </button>
          <button class="btn primary" type="submit" disabled={saving || !dirty}>
            {saving ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    {/if}
  </div>
</div>

<style>
  .settings { max-width: 620px; }

  /* Two dozen settings is a long scroll, and a Save button at the bottom of it
     is one an operator has to go looking for after every change. */
  .settings :global(.modal-actions) {
    position: sticky;
    bottom: 0;
    margin: 4px -20px -20px;
    padding: 12px 20px;
    background: var(--panel);
    border-top: 1px solid var(--rule);
  }

  section + section { margin-top: 26px; }
  /* Sections and fields both label themselves in mono, which is the house
     style, so the heading earns its rank on weight and rule rather than on a
     typeface the rest of the app does not use. */
  .group {
    margin: 0 0 16px;
    padding-bottom: 7px;
    border-bottom: 1px solid var(--rule);
    color: var(--ink);
    letter-spacing: .22em;
  }

  /* A pair marked half sits beside the next one, so two settings that are only
     meaningful against each other are read together. */
  .fields { display: flex; flex-wrap: wrap; column-gap: 16px; }
  .fields > :global(*) { flex: 1 1 100%; min-width: 0; }
  .fields > .field.half { flex: 1 1 calc(50% - 8px); }
  @media (max-width: 560px) {
    .fields > .field.half { flex-basis: 100%; }
  }

  .hint {
    display: block;
    margin-top: 5px;
    font-size: 11.5px;
    line-height: 1.45;
    color: var(--ink-3);
  }
  .hint.warn { color: var(--judge); }
  .hint .mono { font-size: 11px; }

  .switch {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    margin-bottom: 14px;
    cursor: pointer;
  }
  .switch input { margin-top: 2px; accent-color: var(--live); }
  .switch-label {
    font-family: var(--f-mono);
    font-size: 10.5px;
    letter-spacing: .15em;
    text-transform: uppercase;
    color: var(--ink-3);
  }
  .switch .hint { margin-top: 3px; }

  .secret { display: flex; gap: 6px; align-items: center; }
  .secret input { min-width: 0; }
  /* A set secret is shown as filled rather than empty, since its placeholder is
     standing in for a value that exists and is simply not ours to display. */
  .secret input.filled::placeholder { color: var(--ink-2); letter-spacing: .06em; }
  .secret input.filled { border-color: var(--ink-3); }

  input[type="number"] {
    width: 100%;
    padding: 8px 10px;
    border: 1px solid var(--rule);
    border-radius: var(--r);
    background: var(--panel);
    color: var(--ink);
    font-family: var(--f-ui);
    font-size: 13.5px;
  }
  input.mono { font-family: var(--f-mono); font-size: 12.5px; }
</style>
