<script>
  import { createEventDispatcher, onDestroy } from 'svelte';
  import { AuthError, breakdown, fetchGroupPlan, stopGroup } from '../api.js';
  import { collectSeed, describeSeed } from '../seed.js';
  import ModelPicker from './ModelPicker.svelte';

  const dispatch = createEventDispatcher();

  // Two phases in one dialog: the idea goes in, and what came back stays on
  // screen while it runs. Closing early would hide the only view of the plan.
  let idea = $state('');
  let model = $state('');
  let loading = $state(false);
  let error = $state('');
  let result = $state(null);
  let plan = $state(null);
  let poll = null;

  // The seed is read here and travels in the request body, so what the planner
  // is shown is settled before the slow call starts.
  let seed = $state([]);
  let skipped = $state([]);
  let seedError = $state('');
  let fileInput = $state(null);
  let dirInput = $state(null);

  let byId = $derived(new Map((plan?.tasks ?? []).map(t => [t.id, t])));
  let waves = $derived(plan?.waves ?? []);
  let doneCount = $derived((plan?.tasks ?? []).filter(t => t.status === 'done').length);

  onDestroy(stopPolling);

  function stopPolling() {
    if (poll) clearInterval(poll);
    poll = null;
  }

  async function submit() {
    if (!idea.trim()) {
      error = 'Describe the idea before splitting it.';
      return;
    }
    loading = true;
    error = '';
    try {
      // The server does one or two model calls before it answers, so this is
      // the one request in the UI measured in tens of seconds.
      result = await breakdown({ idea, model, start: true, seed });
      plan = result.plan ?? null;
      dispatch('created');
      if (result.group_id) startPolling(result.group_id);
    } catch (e) {
      error = e instanceof AuthError ? 'The session expired. Log in again to continue.' : e.message;
    }
    loading = false;
  }

  // A pick adds to what is already there, so files and a folder can both go in.
  // The input is cleared afterwards or picking the same path twice is a no-op.
  async function addFiles(event) {
    const picked = [...(event.target.files ?? [])];
    event.target.value = '';
    if (!picked.length) return;
    seedError = '';
    try {
      const next = await collectSeed(picked, seed);
      seed = next.files;
      skipped = next.skipped;
    } catch (e) {
      seedError = e.message;
    }
  }

  function removeSeed(path) {
    seed = seed.filter(f => f.path !== path);
  }

  function clearSeed() {
    seed = [];
    skipped = [];
    seedError = '';
  }

  function startPolling(groupId) {
    stopPolling();
    poll = setInterval(async () => {
      try {
        plan = await fetchGroupPlan(groupId);
        dispatch('created'); // refresh the board behind the dialog
        if (!plan.running) stopPolling();
      } catch (e) {
        error = e.message;
        stopPolling();
      }
    }, 2000);
  }

  async function handleStop() {
    if (!result?.group_id) return;
    try {
      plan = await stopGroup(result.group_id);
      stopPolling();
      dispatch('created');
    } catch (e) {
      error = e.message;
    }
  }

  function close() {
    stopPolling();
    dispatch('close');
  }

  function pad(n) { return String(n).padStart(2, '0'); }

  // The ordinal is the subtask's place in the schedule, which is the same
  // number the plan graph puts on its node.
  let ordinals = $derived.by(() => {
    const out = new Map();
    let n = 0;
    for (const wave of waves) for (const id of wave) out.set(id, ++n);
    return out;
  });
</script>

<div class="modal-backdrop">
  <div class="modal" role="dialog" aria-modal="true" aria-label="Break down an idea">
    <div class="modal-head">
      <div class="eyebrow">One idea, many owners</div>
      <h2>Break down an idea</h2>
    </div>

    <div class="modal-body">
    {#if !result}
      <form onsubmit={(e) => { e.preventDefault(); submit(); }}>
        <label class="field">
          <span class="eyebrow">Idea</span>
          <textarea
            bind:value={idea}
            rows="4"
            disabled={loading}
            placeholder="One thing to build. It is split into subtasks that own different files and run in parallel."
          ></textarea>
        </label>
        <div class="field">
          <span class="eyebrow">Seed</span>
          <div class="seed-pick">
            <button class="btn tiny" type="button" disabled={loading} onclick={() => fileInput?.click()}>
              Add files
            </button>
            <button class="btn tiny" type="button" disabled={loading} onclick={() => dirInput?.click()}>
              Add folder
            </button>
            {#if seed.length}
              <span class="seed-tally">{describeSeed(seed)}</span>
              <button class="btn tiny quiet" type="button" disabled={loading} onclick={clearSeed}>Clear</button>
            {/if}
          </div>
          <input class="hidden-input" type="file" multiple bind:this={fileInput} onchange={addFiles} />
          <input class="hidden-input" type="file" webkitdirectory bind:this={dirInput} onchange={addFiles} />

          {#if seed.length}
            <ul class="seed-list">
              {#each seed as file (file.path)}
                <li>
                  <span class="path">{file.path}</span>
                  <button class="drop" type="button" disabled={loading} aria-label="Remove {file.path}" onclick={() => removeSeed(file.path)}>×</button>
                </li>
              {/each}
            </ul>
          {/if}
          {#if skipped.length}
            <div class="notice">
              <strong>Skipped</strong>
              {#each skipped as s (s.path)}<span class="skip">{s.path} — {s.reason}</span>{/each}
            </div>
          {/if}
          {#if seedError}
            <div class="notice bad">{seedError}</div>
          {/if}
        </div>
        <label class="field">
          <span class="eyebrow">Model</span>
          <ModelPicker bind:value={model} disabled={loading} />
        </label>
        {#if error}
          <div class="notice bad">{error}</div>
        {/if}
        <p class="hint">
          Each subtask declares the paths it writes and reads. A subtask that reads
          another's output waits for it, so the order comes from the files rather
          than from a list. An idea that will not partition runs as one task.
          {#if seed.length}
            The planner is shown the seed, so name those files in the idea to say
            what to read.
          {/if}
        </p>
        <div class="modal-actions">
          <button class="btn" type="button" onclick={close} disabled={loading}>Cancel</button>
          <button class="btn primary" type="submit" disabled={loading}>
            {loading ? 'Splitting…' : 'Split and run'}
          </button>
        </div>
      </form>

    {:else if result.fallback}
      <!-- The floor: the idea did not divide, so it runs as one task. -->
      <div class="notice">{result.fallback}</div>
      <div class="rows">
        {#each result.tasks as task (task.id)}
          <div class="subtask">
            <span class="mark {task.status}"></span>
            <span class="name">{task.title}</span>
            <span class="state {task.status}">{task.status}</span>
          </div>
        {/each}
      </div>
      <div class="modal-actions">
        <button class="btn primary" type="button" onclick={close}>Close</button>
      </div>

    {:else}
      <dl class="tally">
        <div><dt class="eyebrow">Subtasks</dt><dd>{plan?.tasks?.length ?? 0}</dd></div>
        <div><dt class="eyebrow">Waves</dt><dd>{waves.length}</dd></div>
        <div><dt class="eyebrow">Done</dt><dd>{doneCount}</dd></div>
        <div>
          <dt class="eyebrow">State</dt>
          <dd class="state {plan?.running ? 'running' : 'done'}">
            <span class="mark {plan?.running ? 'running' : 'done'}"></span>{plan?.running ? 'running' : 'settled'}
          </dd>
        </div>
      </dl>

      {#each waves as wave, i}
        <div class="wave">
          <div class="eyebrow wave-label">Wave {i + 1}</div>
          <div class="rows">
            {#each wave as id (id)}
              {@const task = byId.get(id)}
              {#if task}
                <button class="subtask link" onclick={() => { close(); dispatch('openTask', task.id); }}>
                  <span class="mark {task.status}"></span>
                  <span class="ord">{pad(ordinals.get(id) ?? 0)}</span>
                  <span class="name">{task.title}</span>
                  <span class="state {task.status}">{task.status}</span>
                </button>
              {/if}
            {/each}
          </div>
        </div>
      {/each}

      {#if error}
        <div class="notice bad">{error}</div>
      {/if}
      <div class="modal-actions">
        {#if plan?.running}
          <button class="btn" type="button" onclick={handleStop}>Stop plan</button>
        {/if}
        <button class="btn primary" type="button" onclick={close}>Close</button>
      </div>
    {/if}
    </div>
  </div>
</div>

<style>
  .modal { max-width: 500px; }

  .hint {
    margin: 0 0 4px;
    font-size: 12px;
    line-height: 1.55;
    color: var(--ink-2);
  }

  .seed-pick { display: flex; align-items: center; gap: 6px; }
  .seed-tally { font-family: var(--f-mono); font-size: 11px; color: var(--ink-3); }
  .hidden-input { display: none; }

  .seed-list {
    margin: 7px 0 0;
    padding: 0;
    list-style: none;
    max-height: 132px;
    overflow-y: auto;
    border: 1px solid var(--rule);
  }
  .seed-list li {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px 4px 9px;
    font-family: var(--f-mono);
    font-size: 11.5px;
  }
  .seed-list li + li { border-top: 1px solid var(--rule); }
  .path { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .drop {
    background: none;
    border: none;
    padding: 0 4px;
    color: var(--ink-3);
    font: inherit;
    font-size: 14px;
    line-height: 1;
    cursor: pointer;
  }
  .drop:hover:not(:disabled) { color: var(--fault); }

  .skip { display: block; font-family: var(--f-mono); font-size: 11px; }

  .tally {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    margin: 0 0 18px;
    border: 1px solid var(--rule);
  }
  .tally > div { padding: 8px 10px; }
  .tally > div + div { border-left: 1px solid var(--rule); }
  .tally dt { margin: 0 0 3px; }
  .tally dd { margin: 0; font-family: var(--f-mono); font-size: 13px; }

  .wave { margin-bottom: 14px; }
  .wave-label { display: block; margin-bottom: 6px; }
  .rows { display: flex; flex-direction: column; gap: 5px; }

  .subtask {
    display: flex;
    width: 100%;
    align-items: center;
    gap: 9px;
    padding: 8px 10px;
    background: var(--panel);
    border: 1px solid var(--rule);
    color: var(--ink);
    font: inherit;
    font-size: 13px;
    text-align: left;
  }
  .subtask.link { cursor: pointer; transition: border-color .12s; }
  .subtask.link:hover { border-color: var(--ink); }
  .ord { font-family: var(--f-mono); font-size: 10.5px; color: var(--ink-3); }
  .name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  .notice { margin-bottom: 12px; }
</style>
