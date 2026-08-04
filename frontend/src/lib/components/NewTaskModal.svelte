<script>
  import { createEventDispatcher } from 'svelte';
  import { createTask } from '../api.js';
  import { settings } from '../config.svelte.js';
  import ModelPicker from './ModelPicker.svelte';

  let dispatch = createEventDispatcher();

  let title = $state('');
  let description = $state('');
  let goal = $state('');
  let criteria = $state('');
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
      await createTask({ title, description, goal, criteria, model });
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
      <!-- Only worth asking for where something will check them. A breakdown
           writes its own; a task made by hand is judged on its goal alone
           unless somebody says what "done" means here. -->
      {#if settings.review}
        <label class="field">
          <span class="eyebrow">Acceptance criteria · one per line</span>
          <textarea
            bind:value={criteria}
            rows="3"
            placeholder="the page opens from file:// with no console errors&#10;parse(&quot;3 4 +&quot;) returns 7"
          ></textarea>
          <span class="hint">A reviewer checks the output against these. Write what can be answered yes or no.</span>
        </label>
      {/if}
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
  .hint { display: block; margin-top: 5px; font-size: 11.5px; line-height: 1.45; color: var(--ink-3); }
</style>
