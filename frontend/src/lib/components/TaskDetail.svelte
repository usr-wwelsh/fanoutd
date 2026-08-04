<script>
  import TraceView from './TraceView.svelte';
  import ModelPicker from './ModelPicker.svelte';
  import ContinueModal from './ContinueModal.svelte';
  import ReviewBadge from './ReviewBadge.svelte';
  import { createEventDispatcher } from 'svelte';
  import {
    moveTask, startAgent, stopAgent, fetchFiles, fetchGroupPlan, updateTask,
    retryTask, rawFileUrl, previewUrl, fileUrl,
  } from '../api.js';
  import { criteriaLines, isRework, reviewState } from '../review.js';
  import { settings } from '../config.svelte.js';

  // focus is what the panel was opened to answer. Only the trace acts on it, by
  // opening on the reviewer's half when a verdict is what was clicked.
  let { task, tasks = [], focus = null } = $props();

  const dispatch = createEventDispatcher();

  let busy = $state(false);
  let actionError = $state('');
  let files = $state([]);
  let showContinue = $state(false);
  let copied = $state('');

  // Subtasks of a breakdown share one workspace, so the listing holds their
  // siblings' output too. The count is what this task itself wrote.
  let mine = $derived(files.filter(f => f.owned));

  let running = $derived(task.status === 'running');
  let parent = $derived(task.parent_id ? tasks.find(t => t.id === task.parent_id) : null);
  let siblings = $derived(
    tasks.filter(t => t.id !== task.id && t.workspace_id && t.workspace_id === task.workspace_id)
  );

  const statusLabels = {
    idle: 'Not started',
    running: 'Agent is working',
    done: 'Finished',
    stopped: 'Stopped',
    error: 'Failed',
  };

  // Done in the review column is not finished, and saying so was the whole point
  // of splitting the two: the agent stopped, and whether the work is any good is
  // the question that has not been answered yet.
  let statusLabel = $derived(
    task.column === 'review' && task.status === 'done'
      ? 'Run finished, not yet judged'
      : task.column === 'review' && task.status === 'running'
        ? 'Being reviewed'
        : statusLabels[task.status] ?? task.status
  );

  let review = $derived(reviewState(task));
  let criteria = $derived(criteriaLines(task.criteria));
  let rework = $derived(isRework(task));
  let reworks = $derived(tasks.filter(t => t.parent_id === task.id && isRework(t)));

  // A breakdown is judged whole, and one verdict is recorded against the subtask
  // that ran last — so the reviewer's trace is on a sibling and every other
  // subtask has to say where. Resolved from the plan rather than guessed, and
  // only for a task that belongs to one.
  let anchorId = $state('');
  $effect(() => {
    const groupId = task.group_id;
    anchorId = '';
    if (!groupId) return;
    let cancelled = false;
    fetchGroupPlan(groupId)
      .then(plan => {
        const last = plan.waves?.[plan.waves.length - 1] ?? [];
        if (!cancelled) anchorId = last[last.length - 1] ?? '';
      })
      .catch(() => {});
    return () => { cancelled = true; };
  });
  let anchor = $derived(
    anchorId && anchorId !== task.id ? tasks.find(t => t.id === anchorId) : null
  );

  $effect(() => {
    const id = task.id;
    let cancelled = false;
    const load = async () => {
      try {
        const list = await fetchFiles(id);
        if (!cancelled) files = list;
      } catch (e) {
        console.error('Failed to load files', e);
      }
    };
    load();
    const interval = setInterval(load, task.status === 'running' ? 2000 : 10000);
    return () => { cancelled = true; clearInterval(interval); };
  });

  async function act(fn) {
    busy = true;
    actionError = '';
    try {
      await fn();
    } catch (e) {
      actionError = e.message;
    }
    busy = false;
    dispatch('refreshed', task);
  }

  const start = () => act(() => startAgent(task.id));
  const stop = () => act(() => stopAgent(task.id));
  const moveTo = (column) => act(() => moveTask(task.id, column));

  function setModel(model) {
    if (model === (task.model ?? '')) return;
    act(() => updateTask(task.id, { model }));
  }

  // Retry clones the brief into a clean workspace; the current run is kept as-is.
  function retry() {
    act(async () => {
      const created = await retryTask(task.id, { start: true });
      dispatch('openTask', created.id);
    });
  }

  // Open serves the file from the workspace, so a page loads with the scripts
  // and styles beside it. The file name opens the raw bytes instead.
  function open(file) {
    window.open(previewUrl(task.id, file.path), '_blank', 'noopener');
  }

  function openRaw(file) {
    window.open(rawFileUrl(task.id, file.path), '_blank', 'noopener');
  }

  async function copyPath(file) {
    try {
      await navigator.clipboard.writeText(fileUrl(file.abs));
      copied = file.path;
      setTimeout(() => { if (copied === file.path) copied = ''; }, 1500);
    } catch (e) {
      actionError = 'Could not copy to clipboard: ' + e.message;
    }
  }
</script>

<div class="detail">
  <div class="eyebrow">Task</div>
  <h2>{task.title}</h2>
  {#if task.description}
    <p class="description">{task.description}</p>
  {/if}
  {#if task.goal}
    <div class="block">
      <h3 class="eyebrow">Goal</h3>
      <p class="idea goal-text">{task.goal}</p>
    </div>
  {/if}

  <!-- What the output is held to, settled before the work started and shown to
       the agent and the reviewer in these same words. Worth showing even with
       review off: they are the only checkable statement of what "done" means. -->
  {#if criteria.length}
    <div class="block">
      <h3 class="eyebrow">Acceptance criteria · {criteria.length}</h3>
      <ul class="criteria">
        {#each criteria as line, i (i)}
          <li>{line}</li>
        {/each}
      </ul>
    </div>
  {:else if settings.review}
    <div class="block">
      <h3 class="eyebrow">Acceptance criteria</h3>
      <p class="none">None recorded, so a review holds this to its goal and to nothing more. A breakdown writes them itself.</p>
    </div>
  {/if}

  <div class="status-line {task.status}">
    <span class="mark {task.status}"></span>
    <span>{statusLabel}</span>
  </div>

  {#if parent}
    <div class="lineage">
      {rework ? `Rework ${task.review_round} of` : task.workspace_id === parent.workspace_id ? 'Continues' : 'Retry of'}
      <button class="link" onclick={() => dispatch('openTask', parent.id)}>{parent.title}</button>
    </div>
  {/if}
  {#if reworks.length}
    <div class="lineage">
      Sent back, and repaired by
      {#each reworks as r (r.id)}
        <button class="link" onclick={() => dispatch('openTask', r.id)}>{r.title}</button>
      {/each}
    </div>
  {/if}

  {#if review}
    <div class="notice {review.tone === 'done' ? '' : review.tone === 'fault' ? 'bad' : 'judge'} verdict-block">
      <div class="verdict-head">
        <ReviewBadge state={review} title={false} />
        {#if task.review_round > 0}<span class="round">round {task.review_round}</span>{/if}
      </div>
      <p class="verdict-note">{task.verdict_note || review.hint}</p>
      {#if anchor}
        <button class="link" onclick={() => dispatch('openReview', anchor.id)}>
          The reviewer's steps are on {anchor.title}
        </button>
      {/if}
    </div>
  {/if}

  <div class="block">
    <h3 class="eyebrow">Model</h3>
    <ModelPicker value={task.model ?? ''} disabled={busy || running} onselect={setModel} />
  </div>

  <div class="actions">
    {#if running}
      <button onclick={stop} disabled={busy} class="btn live">Stop agent</button>
    {:else}
      <button onclick={start} disabled={busy} class="btn primary">
        {task.status === 'idle' ? 'Start agent' : 'Resume agent'}
      </button>
      <button onclick={retry} disabled={busy} class="btn" title="Run this same brief again in a clean workspace. This task is kept.">
        Retry from scratch
      </button>
      <button onclick={() => showContinue = true} disabled={busy} class="btn" title="Give the agent a new goal on this workspace, keeping the files.">
        New goal here
      </button>
    {/if}
    {#if task.column !== 'ideas'}
      <button onclick={() => moveTo('ideas')} disabled={busy} class="btn quiet">Move to Ideas</button>
    {/if}
    {#if task.column !== 'finished'}
      <button onclick={() => moveTo('finished')} disabled={busy} class="btn quiet">Move to Finished</button>
    {/if}
    <button
      class="btn danger delete-btn"
      disabled={busy}
      onclick={() => dispatch('deleteTask', { taskId: task.id, title: task.title })}
      title="Delete this task, its trace, and its output files"
    >Delete</button>
  </div>

  {#if actionError}
    <div class="notice bad"><strong>That request failed.</strong> {actionError}</div>
  {/if}

  {#if task.status === 'error' && task.error}
    <div class="notice bad"><strong>The agent stopped with an error.</strong> {task.error}</div>
  {/if}

  {#if task.summary}
    <div class="block">
      <h3 class="eyebrow">Summary</h3>
      <p class="summary-text">{task.summary}</p>
    </div>
  {/if}

  <div class="block files">
    <h3 class="eyebrow">
      Output files · {mine.length}{#if files.length > mine.length} · {files.length - mine.length} from siblings{/if}
    </h3>
    {#if files.length === 0}
      <div class="no-files">Nothing written yet. Files appear here as the agent creates them.</div>
    {:else}
      <ul>
        {#each files as file (file.path)}
          <li class:sibling={!file.owned}>
            <button class="file-path" onclick={() => openRaw(file)} title="Open the raw file in a new tab">{file.path}</button>
            {#if !file.owned}
              <span class="file-owner" title="Written by another subtask sharing this workspace">sibling</span>
            {/if}
            <span class="file-size">{file.size} B</span>
            <button class="btn tiny" onclick={() => open(file)} title="Open it served from its workspace, so a page finds its own scripts and styles">Open</button>
            <button class="copy-btn" onclick={() => copyPath(file)} title={fileUrl(file.abs)}>
              {copied === file.path ? 'Copied' : 'file://'}
            </button>
          </li>
        {/each}
      </ul>
      <div class="files-hint">{files[0].abs.replace(/\/[^/]*$/, '/')}</div>
    {/if}
    {#if siblings.length > 0}
      <div class="shared">
        Workspace shared with
        {#each siblings as s (s.id)}
          <button class="link" onclick={() => dispatch('openTask', s.id)}>{s.title}</button>
        {/each}
      </div>
    {/if}
  </div>

  <TraceView taskId={task.id} live={running} {focus} />
</div>

{#if showContinue}
  <ContinueModal
    task={task}
    files={files}
    on:close={() => showContinue = false}
    on:created={(e) => { showContinue = false; dispatch('openTask', e.detail.id); }}
  />
{/if}

<style>
  .detail h2 {
    margin: 5px 0 12px;
    font-size: 19px;
    font-weight: 600;
    line-height: 1.25;
    letter-spacing: -.01em;
    padding-right: 24px;
  }
  .description {
    margin: 0 0 16px;
    font-size: 13px;
    line-height: 1.55;
    color: var(--ink-2);
  }

  .block {
    margin-bottom: 14px;
    padding: 10px 12px;
    background: var(--sunk);
    border: 1px solid var(--rule-soft);
  }
  .block h3 { margin: 0 0 6px; font-weight: 400; }
  .block p { margin: 0; }
  .goal-text { font-size: 15px; line-height: 1.4; }
  .summary-text { font-size: 13px; line-height: 1.55; color: var(--ink-2); }
  .none { font-size: 12px; line-height: 1.5; color: var(--ink-3); }

  .criteria { margin: 0; padding-left: 16px; }
  .criteria li {
    font-size: 12.5px;
    line-height: 1.5;
    color: var(--ink-2);
    margin-bottom: 3px;
  }
  .criteria li::marker { color: var(--ink-3); }

  .verdict-block { margin-bottom: 14px; }
  .verdict-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .verdict-note {
    margin: 7px 0 0;
    font-size: 12.5px;
    line-height: 1.55;
    color: var(--ink-2);
    white-space: pre-wrap;
  }
  .verdict-block .link { margin: 8px 0 0; display: inline-block; }

  .status-line {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    margin-bottom: 14px;
    border: 1px solid var(--rule);
    border-left: 3px solid var(--rule);
    font-family: var(--f-mono);
    font-size: 11px;
    letter-spacing: .12em;
    text-transform: uppercase;
    color: var(--ink-2);
  }
  .status-line.running { border-left-color: var(--live); color: var(--live); }
  .status-line.error { border-left-color: var(--fault); color: var(--fault); }
  .status-line.done { border-left-color: var(--ink); color: var(--ink); }

  .lineage { font-size: 12px; color: var(--ink-3); margin-bottom: 14px; }
  .link {
    background: none;
    border: none;
    padding: 0;
    margin-left: 4px;
    color: var(--live);
    cursor: pointer;
    font: inherit;
    text-decoration: underline;
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin-bottom: 14px;
  }
  .delete-btn { margin-left: auto; }

  .notice { margin-bottom: 14px; }

  .files ul { list-style: none; margin: 0; padding: 0; }
  .files li {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 3px 0;
  }
  /* Still listed and still openable — the deliverable is often a sibling's
     index page — but not counted as this task's work. */
  .files li.sibling .file-path { color: var(--ink-3); }
  .file-owner {
    font-size: 10px;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    color: var(--ink-3);
    border: 1px solid var(--rule-soft);
    border-radius: var(--r);
    padding: 0 4px;
  }
  .file-path {
    flex: 1;
    min-width: 0;
    text-align: left;
    background: none;
    border: none;
    padding: 0;
    font-family: var(--f-mono);
    font-size: 11.5px;
    color: var(--ink);
    word-break: break-all;
    cursor: pointer;
  }
  .file-path:hover { color: var(--live); text-decoration: underline; }
  .file-size {
    flex: none;
    font-family: var(--f-mono);
    font-size: 10.5px;
    color: var(--ink-3);
    white-space: nowrap;
  }
  .copy-btn {
    flex: none;
    border: 1px solid var(--rule);
    background: none;
    color: var(--ink-3);
    padding: 3px 7px;
    font-family: var(--f-mono);
    font-size: 10px;
    cursor: pointer;
    white-space: nowrap;
  }
  .copy-btn:hover { border-color: var(--ink); color: var(--ink); }

  .no-files { font-size: 12px; color: var(--ink-3); line-height: 1.5; }
  .files-hint {
    margin-top: 8px;
    font-family: var(--f-mono);
    font-size: 10.5px;
    color: var(--ink-3);
    word-break: break-all;
  }
  .shared {
    margin-top: 10px;
    padding-top: 8px;
    border-top: 1px solid var(--rule-soft);
    font-size: 12px;
    color: var(--ink-3);
  }
</style>
