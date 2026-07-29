<script>
  import { fetchModels } from '../api.js';

  // value is '' for "use the server default". onselect fires only on a real
  // user change, so a parent can persist it without echoing its own updates.
  let { value = $bindable(''), disabled = false, onselect } = $props();

  let models = $state([]);
  let defaultModel = $state('');
  let error = $state('');
  let loading = $state(true);

  let free = $derived(models.filter(m => m.free));
  let paid = $derived(models.filter(m => !m.free));

  $effect(() => {
    let cancelled = false;
    fetchModels()
      .then(res => {
        if (cancelled) return;
        models = res.models ?? [];
        defaultModel = res.default ?? '';
      })
      .catch(e => { if (!cancelled) error = e.message; })
      .finally(() => { if (!cancelled) loading = false; });
    return () => { cancelled = true; };
  });

  function label(m) {
    const ctx = m.context_length ? `${Math.round(m.context_length / 1000)}k` : '';
    return [m.name, ctx, m.tools ? 'tools' : ''].filter(Boolean).join(' · ');
  }
</script>

<div class="model-picker">
  <select bind:value disabled={disabled || loading} onchange={(e) => onselect?.(e.currentTarget.value)}>
    <option value="">Default{defaultModel ? ` (${defaultModel})` : ''}</option>
    {#if free.length}
      <optgroup label="Free">
        {#each free as m (m.id)}
          <option value={m.id}>{label(m)}</option>
        {/each}
      </optgroup>
    {/if}
    {#if paid.length}
      <optgroup label="Paid">
        {#each paid as m (m.id)}
          <option value={m.id}>{label(m)}</option>
        {/each}
      </optgroup>
    {/if}
  </select>
  {#if loading}
    <div class="note">Loading models…</div>
  {:else if error}
    <div class="note error">Models did not load: {error}</div>
  {:else if value}
    <div class="note mono">{value}</div>
  {/if}
</div>

<style>
  select { font-size: 13px; }
  .note {
    margin-top: 5px;
    font-family: var(--f-mono);
    font-size: 10.5px;
    color: var(--ink-3);
  }
  .note.mono { word-break: break-all; }
  .note.error { color: var(--fault); }
</style>
