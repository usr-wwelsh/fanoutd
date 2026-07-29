<script>
  import { createEventDispatcher, untrack } from 'svelte';
  import { continueTask } from '../api.js';
  import ModelPicker from './ModelPicker.svelte';

  let { task, files = [] } = $props();

  const dispatch = createEventDispatcher();

  let title = $state('');
  let description = $state('');
  let goal = $state('');
  // Seeded from the parent task; the modal is remounted per open, so it does
  // not need to track later changes.
  let model = $state(untrack(() => task.model ?? ''));
  let start = $state(true);
  let loading = $state(false);
  let error = $state('');

  async function handleSubmit() {
    if (!goal.trim()) {
      error = 'Describe the new goal before continuing.';
      return;
    }
    loading = true;
    error = '';
    try {
      const created = await continueTask(task.id, { title, description, goal, model, start });
      dispatch('created', created);
    } catch (e) {
      error = e.message;
    }
    loading = false;
  }
</script>

<div class="modal-backdrop">
  <div class="modal" role="dialog" aria-modal="true" aria-label="New goal in this workspace">
    <div class="modal-head">
      <div class="eyebrow">Same files, fresh context</div>
      <h2>New goal here</h2>
    </div>
    <div class="modal-body">
    <p class="lede">
      Creates a new task on <strong>{task.title}</strong>'s workspace. The agent starts fresh
      with no conversation history, but every file below is still there.
    </p>

    <div class="carryover">
      {#if files.length === 0}
        <span class="empty">This workspace is empty.</span>
      {:else}
        {#each files as file (file.path)}
          <span class="chip">{file.path}</span>
        {/each}
      {/if}
    </div>

    <form onsubmit={(e) => { e.preventDefault(); handleSubmit(); }}>
      <label class="field">
        <span class="eyebrow">New goal</span>
        <textarea bind:value={goal} rows="3" placeholder="What should the agent do next with these files?"></textarea>
      </label>
      <label class="field">
        <span class="eyebrow">Title</span>
        <input type="text" bind:value={title} placeholder="Optional — defaults to a numbered follow-up" />
      </label>
      <label class="field">
        <span class="eyebrow">Description</span>
        <textarea bind:value={description} rows="2" placeholder="Optional context for the agent"></textarea>
      </label>
      <label class="field">
        <span class="eyebrow">Model</span>
        <ModelPicker bind:value={model} disabled={loading} />
      </label>
      <label class="check">
        <input type="checkbox" bind:checked={start} />
        Start the agent right away
      </label>

      {#if error}
        <div class="notice bad">{error}</div>
      {/if}
      <div class="modal-actions">
        <button class="btn" type="button" onclick={() => dispatch('close')} disabled={loading}>Cancel</button>
        <button class="btn primary" type="submit" disabled={loading}>
          {loading ? 'Creating…' : 'Create task'}
        </button>
      </div>
    </form>
    </div>
  </div>
</div>

<style>
  .modal { max-width: 480px; }
  .lede {
    margin: 0 0 12px;
    font-size: 12.5px;
    line-height: 1.55;
    color: var(--ink-2);
  }
  .carryover {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-bottom: 16px;
    padding: 8px;
    background: var(--sunk);
    border: 1px solid var(--rule-soft);
    max-height: 6rem;
    overflow-y: auto;
  }
  .chip {
    font-family: var(--f-mono);
    font-size: 10.5px;
    padding: 2px 6px;
    background: var(--panel);
    border: 1px solid var(--rule-soft);
    color: var(--ink-2);
  }
  .empty { font-family: var(--f-mono); font-size: 11px; color: var(--ink-3); }
  .check {
    display: flex;
    align-items: center;
    gap: 7px;
    margin-bottom: 14px;
    font-size: 12.5px;
    color: var(--ink-2);
  }
  .check input { width: auto; margin: 0; accent-color: var(--live); }
</style>
