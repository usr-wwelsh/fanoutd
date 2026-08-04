<script>
  import { createEventDispatcher } from 'svelte';
  import { fetchFiles, fetchGroupPlan, previewUrl, stopGroup } from '../api.js';
  import { reworkChain, rollupReview } from '../review.js';
  import TaskCard from './TaskCard.svelte';
  import ReviewBadge from './ReviewBadge.svelte';

  // tasks is this column's members of the group, which is usually all of them.
  // A group created before moves were group-wide can still straddle columns, so
  // the header says so rather than reporting a count it is not showing.
  //
  // all is the whole board, and is only used to find the reworks a rejection
  // opened: they are ordinary tasks outside the group, and without this the plan
  // they repair says nothing about them.
  let { groupId, tasks, all = [], selectedId } = $props();

  const dispatch = createEventDispatcher();

  let plan = $state(null);
  // Open while the plan is running, closed once it is not — until the user says
  // otherwise, after which their choice is the one that holds.
  let override = $state(null);
  let showFiles = $state(false);
  let files = $state([]);
  let error = $state('');

  // The plan is the shape — waves and the idea — and changes only when a subtask
  // claims a path nobody predicted. The board already polls the subtasks, so it
  // is re-fetched on a status change rather than on a timer.
  let signature = $derived(tasks.map(t => `${t.id}:${t.status}`).join('|'));
  let loadedFor = null;

  $effect(() => {
    const sig = signature;
    if (sig === loadedFor) return;
    loadedFor = sig;
    loadPlan();
  });

  async function loadPlan() {
    try {
      plan = await fetchGroupPlan(groupId);
      error = '';
    } catch (e) {
      // A group whose plan cannot be resolved still has cards to draw; the
      // header falls back to the subtasks it was handed.
      error = e.message;
    }
    if (showFiles) loadFiles();
  }

  async function loadFiles() {
    try {
      files = await fetchFiles(tasks[0].id);
    } catch (e) {
      error = e.message;
    }
  }

  function toggleFiles(e) {
    e.stopPropagation();
    showFiles = !showFiles;
    if (showFiles) loadFiles();
  }

  let total = $derived(plan?.tasks?.length ?? tasks.length);
  let partial = $derived(total > tasks.length);
  let waveCount = $derived(plan?.waves?.length ?? 0);
  let title = $derived(plan?.idea || 'Breakdown');

  // Waves keep their plan numbering even when this column holds only some of
  // them, so wave 3 is the third wave of the plan and not the third one drawn.
  let waves = $derived(
    (plan?.waves ?? [[...tasks.map(t => t.id)]])
      .map((ids, i) => ({
        number: i + 1,
        tasks: ids.map(id => tasks.find(t => t.id === id)).filter(Boolean),
      }))
      .filter(w => w.tasks.length > 0)
  );

  // One badge for the whole plan, worst-first: a group with a failed subtask is
  // a failed group however many of its siblings finished.
  let rollup = $derived.by(() => {
    const has = s => tasks.some(t => t.status === s);
    if (plan?.running || has('running')) return { label: 'Running', cls: 'running' };
    if (has('error')) return { label: 'Error', cls: 'error' };
    if (has('stopped')) return { label: 'Stopped', cls: 'stopped' };
    if (tasks.every(t => t.status === 'done')) return { label: 'Done', cls: 'done' };
    return { label: 'Idle', cls: 'idle' };
  });

  let doneCount = $derived(tasks.filter(t => t.status === 'done').length);

  // One verdict covers a whole breakdown — the subtasks were split by file, and
  // whether the idea was achieved is a property of what they add up to — so the
  // badge is the plan's and not any one subtask's.
  let review = $derived(rollupReview(tasks));
  let reworks = $derived(reworkChain(tasks.map(t => t.id), all));
  let latestRework = $derived(reworks.length ? reworks[reworks.length - 1] : null);

  // One segment per wave, so a collapsed plan still shows how far through its
  // schedule it is without expanding to the subtasks.
  let waveState = $derived(
    waves.map(w => {
      if (w.tasks.some(t => t.status === 'running')) return 'running';
      if (w.tasks.some(t => t.status === 'error')) return 'error';
      if (w.tasks.every(t => t.status === 'done')) return 'done';
      return 'idle';
    })
  );
  let expanded = $derived(override ?? rollup.cls === 'running');

  async function handleStop(e) {
    e.stopPropagation();
    try {
      plan = await stopGroup(groupId);
      dispatch('refreshed');
    } catch (err) {
      error = err.message;
    }
  }

  function handleDelete(e) {
    e.stopPropagation();
    dispatch('deleteGroup', { groupId, title, count: total });
  }

  function toggle() {
    override = !expanded;
  }

  function handleKeydown(e) {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggle();
    }
  }
</script>

<div
  class="group"
  class:running={rollup.cls === 'running'}
  class:in-review={review && review.tone === 'judge'}
>
  <div
    class="group-header"
    role="button"
    tabindex="0"
    aria-expanded={expanded}
    draggable="true"
    ondragstart={(e) => e.dataTransfer.setData('text/plain', `group:${groupId}`)}
    onclick={toggle}
    onkeydown={handleKeydown}
  >
    <div class="group-title">
      <span class="caret" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
      <span class="name idea">{title}</span>
      <button
        class="x-btn"
        title="Delete this plan and all {total} subtasks"
        aria-label="Delete plan {title}"
        onclick={handleDelete}
      >✕</button>
    </div>
    {#if waveState.length}
      <div class="wave-bar" aria-hidden="true">
        {#each waveState as w}<i class={w}></i>{/each}
      </div>
    {/if}
    <div class="group-meta">
      <span class="counts">
        {tasks.length}{#if partial} of {total}{/if} subtask{total === 1 ? '' : 's'}
        {#if waveCount > 0}· {waveCount} wave{waveCount === 1 ? '' : 's'}{/if}
        {#if rollup.cls === 'running'}· {doneCount}/{tasks.length} done{/if}
      </span>
      <span class="state {rollup.cls}"><span class="mark {rollup.cls}"></span>{rollup.label}</span>
    </div>
    {#if review}
      <div class="review-line">
        <ReviewBadge state={review} />
        {#if latestRework}
          <button
            class="rework-link"
            onclick={(e) => { e.stopPropagation(); dispatch('selectTask', { taskId: latestRework.id }); }}
          >rework {latestRework.review_round} →</button>
        {/if}
      </div>
    {/if}
    <div class="group-actions">
      {#if plan?.running}
        <button class="btn tiny" onclick={handleStop}>Stop plan</button>
      {/if}
      <button class="btn tiny" onclick={toggleFiles}>{showFiles ? 'Hide files' : 'Files'}</button>
    </div>
    {#if partial}
      <div class="split">The rest of this plan is in another column.</div>
    {/if}
  </div>

  {#if error}
    <div class="error">{error}</div>
  {/if}

  {#if showFiles}
    <!-- Every subtask writes into one workspace, so this is the plan's output
         rather than any one subtask's. -->
    <div class="files">
      {#if files.length === 0}
        <div class="empty">No files yet.</div>
      {:else}
        {#each files as file (file.path)}
          <button class="file" onclick={() => window.open(previewUrl(tasks[0].id, file.path), '_blank', 'noopener')}>
            <span class="file-path">{file.path}</span>
            <span class="file-size">{file.size} B</span>
          </button>
        {/each}
      {/if}
    </div>
  {/if}

  {#if expanded}
    <div class="group-body">
      {#each waves as wave (wave.number)}
        <div class="wave-label eyebrow">Wave {wave.number}</div>
        {#each wave.tasks as task (task.id)}
          <TaskCard
            {task}
            tasks={all}
            isSelected={selectedId === task.id}
            on:select={() => dispatch('selectTask', { taskId: task.id })}
            on:delete={() => dispatch('deleteTask', { taskId: task.id, title: task.title })}
          />
        {/each}
      {/each}
    </div>
  {/if}
</div>

<style>
  .group {
    background: var(--panel);
    border: 1px solid var(--rule);
    border-left: 3px solid var(--rule);
  }
  .group.running { border-left-color: var(--live); }
  .group.in-review:not(.running) { border-left-color: var(--judge); }

  .review-line {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    margin-top: 8px;
  }
  .rework-link {
    background: none;
    border: none;
    padding: 0;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .1em;
    text-transform: uppercase;
    color: var(--judge);
    cursor: pointer;
  }
  .rework-link:hover { text-decoration: underline; }
  .group-header { padding: 10px 12px; cursor: grab; }
  .group-title {
    display: flex;
    align-items: start;
    gap: 6px;
    font-size: 14px;
  }
  .caret { color: var(--ink-3); font-size: 11px; line-height: 1.6; }
  .name {
    flex: 1;
    min-width: 0;
    line-height: 1.3;
    display: -webkit-box;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .group-header:hover .x-btn { opacity: 1; }

  .wave-bar { display: flex; gap: 3px; margin-top: 9px; }
  .wave-bar i { flex: 1; height: 4px; background: var(--rule-soft); }
  .wave-bar i.done { background: var(--ink); }
  .wave-bar i.running { background: var(--live); }
  .wave-bar i.error { background: var(--fault); }

  .group-meta {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    margin-top: 8px;
  }
  .counts { font-family: var(--f-mono); font-size: 10.5px; color: var(--ink-3); }

  .group-actions { display: flex; gap: 5px; margin-top: 9px; }

  .split { margin-top: 7px; font-size: 11.5px; color: var(--ink-2); }
  .error { margin: 0 12px 8px; font-size: 11.5px; color: var(--fault); }

  .files { margin: 0 12px 8px; border-top: 1px solid var(--rule-soft); padding-top: 7px; }
  .file {
    display: flex;
    width: 100%;
    justify-content: space-between;
    gap: 8px;
    padding: 2px 0;
    background: none;
    border: none;
    color: var(--ink-2);
    font-family: var(--f-mono);
    font-size: 11px;
    text-align: left;
    cursor: pointer;
  }
  .file:hover .file-path { color: var(--live); text-decoration: underline; }
  .file-path { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .file-size { flex: none; color: var(--ink-3); }
  .empty { font-family: var(--f-mono); font-size: 11px; color: var(--ink-3); padding: 2px 0; }

  .group-body {
    padding: 0 10px 10px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .wave-label { margin-top: 6px; }
</style>
