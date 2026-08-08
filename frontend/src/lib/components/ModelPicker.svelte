<script>
  import { fetchModels } from '../api.js';

  // value is '' for "use the server default". onselect fires only on a real
  // user change, so a parent can persist it without echoing its own updates.
  let { value = $bindable(''), disabled = false, onselect } = $props();

  let models = $state([]);
  let defaultModel = $state('');
  let provider = $state('');
  // How much the catalog is worth: 'rich' has pricing and tool support, 'bare'
  // is ids only, 'none' is a provider that publishes no list at all.
  let kind = $state('rich');
  let error = $state('');
  let loading = $state(true);

  // Only a rich catalog can be split into tiers. On a bare one every model goes
  // in one ungrouped list, because nothing in the response says which is which.
  let rich = $derived(kind === 'rich');
  let free = $derived(rich ? models.filter(m => m.free) : []);
  let paid = $derived(rich ? models.filter(m => !m.free) : []);

  $effect(() => {
    let cancelled = false;
    fetchModels()
      .then(res => {
        if (cancelled) return;
        models = res.models ?? [];
        defaultModel = res.default ?? '';
        provider = res.provider ?? '';
        kind = res.kind ?? 'rich';
      })
      .catch(e => { if (!cancelled) error = e.message; })
      .finally(() => { if (!cancelled) loading = false; });
    return () => { cancelled = true; };
  });

  function label(m) {
    if (!rich) return m.name;
    const ctx = m.context_length ? `${Math.round(m.context_length / 1000)}k` : '';
    return [m.name, ctx, m.tools ? 'tools' : ''].filter(Boolean).join(' · ');
  }
</script>

<div class="model-picker">
  {#if kind === 'none'}
    <!-- The provider publishes no catalog, so the id is the operator's to give.
         A select with nothing in it would be a dead end; a text field is not. -->
    <input
      type="text"
      bind:value
      {disabled}
      placeholder={defaultModel ? `Default (${defaultModel})` : 'Model id'}
      onchange={(e) => onselect?.(e.currentTarget.value)}
    />
  {:else}
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
      {#if !rich}
        {#each models as m (m.id)}
          <option value={m.id}>{label(m)}</option>
        {/each}
      {/if}
    </select>
  {/if}
  {#if loading}
    <div class="note">Loading models…</div>
  {:else if error}
    <div class="note error">Models did not load: {error}</div>
  {:else if kind !== 'rich'}
    <!-- Say why the extra detail is missing, so its absence reads as the
         provider saying nothing rather than as a model that cannot call tools. -->
    <div class="note">{provider} lists ids only — context and tool support unknown</div>
    {#if value}<div class="note mono">{value}</div>{/if}
  {:else if value}
    <div class="note mono">{value}</div>
  {/if}
</div>

<style>
  select { font-size: 13px; }
  input {
    font-size: 13px;
    font-family: var(--f-mono);
    width: 100%;
  }
  .note {
    margin-top: 5px;
    font-family: var(--f-mono);
    font-size: 10.5px;
    color: var(--ink-3);
  }
  .note.mono { word-break: break-all; }
  .note.error { color: var(--fault); }
</style>
