<script>
  import { createEventDispatcher } from 'svelte';
  import { createTask } from '../api.js';
  import ModelPicker from './ModelPicker.svelte';

  let dispatch = createEventDispatcher();

  let title = $state('');
  let description = $state('');
  let goal = $state('');
  let model = $state('');
  let loading = $state(false);
  let error = $state('');

  async function handleSubmit() {
    if (!title.trim()) {
      error = 'Give the task a title before creating it.';
      return;
    }
    loading = true;
    error = '';
    try {
      await createTask({ title, description, goal, model });
      dispatch('created');
    } catch (e) {
      error = e.message || 'The task could not be created.';
      console.error(e);
    }
    loading = false;
  }
</script>

<div class="modal-backdrop">
  <div class="modal" role="dialog" aria-modal="true" aria-label="New task">
    <div class="modal-head">
      <div class="eyebrow">One task, one workspace</div>
      <h2>New task</h2>
    </div>
    <form class="modal-body" onsubmit={(e) => { e.preventDefault(); handleSubmit(); }}>
      <label class="field">
        <span class="eyebrow">Title</span>
        <input type="text" bind:value={title} required placeholder="Name it in a few words" />
      </label>
      <label class="field">
        <span class="eyebrow">Goal</span>
        <textarea bind:value={goal} rows="3" placeholder="What has to be true for this to be finished?"></textarea>
      </label>
      <label class="field">
        <span class="eyebrow">Description</span>
        <textarea bind:value={description} rows="2" placeholder="Optional context for the agent"></textarea>
      </label>
      <label class="field">
        <span class="eyebrow">Model</span>
        <ModelPicker bind:value={model} disabled={loading} />
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

<style>
  .modal { max-width: 440px; }
</style>
