<script>
  // The plan view is the board turned on its side: every breakdown as the graph
  // it actually is, with the idea it came from above it. Tasks that were never
  // split have no graph to draw, so they sit underneath as a plain list.
  //
  // A rework is never one of those. It is a task the review opened against work
  // that already exists, and listing it on its own would be the one thing this
  // view is meant to prevent — a card with a goal that reads like findings and
  // nothing on the page saying what it is findings about. It is drawn under the
  // plan it repairs, in the plan's own graph.
  import { createEventDispatcher } from 'svelte';
  import { fetchGroupPlan, startGroup, stopGroup } from '../api.js';
  import { isRework, reviewTimeline, reworkChain, rollupReview } from '../review.js';
  import { settings } from '../config.svelte.js';
  import PlanGraph from './PlanGraph.svelte';
  import PlanMinimap from './PlanMinimap.svelte';
  import ReviewBadge from './ReviewBadge.svelte';

  let { tasks, selectedId } = $props();

  const dispatch = createEventDispatcher();

  let plans = $state({});
  let errors = $state({});
  let busy = $state('');
  let root = $state(null);

  let grouped = $derived.by(() => {
    const map = new Map();
    const solo = [];
    for (const task of tasks) {
      if (!task.group_id) { solo.push(task); continue; }
      let group = map.get(task.group_id);
      if (!group) { group = { id: task.group_id, tasks: [] }; map.set(task.group_id, group); }
      group.tasks.push(task);
    }

    // Claimed reworks are drawn by whatever they hang off. Anything left over —
    // a rework whose parent has since been deleted — falls back to a row of its
    // own rather than disappearing from the page.
    const groups = [...map.values()];
    const claimed = new Set();
    for (const group of groups) {
      group.reworks = reworkChain(group.tasks.map(t => t.id), tasks);
      for (const t of group.reworks) claimed.add(t.id);
    }

    const singles = [];
    for (const task of solo) {
      if (claimed.has(task.id)) continue;
      if (isRework(task) && solo.some(t => t.id === task.parent_id)) continue;
      const reworks = reworkChain([task.id], tasks);
      for (const t of reworks) claimed.add(t.id);
      singles.push({ task, reworks });
    }
    return { groups, singles };
  });

  // The subtask a group's verdict was recorded against: the last of the last
  // wave, which is the only one that saw the assembled work and is where the
  // reviewer's trace was written. Without a resolved plan there is no schedule
  // to read it from, so the last subtask created stands in.
  function anchorOf(group) {
    const waves = plans[group.id]?.waves ?? [];
    const last = waves[waves.length - 1] ?? [];
    return last[last.length - 1] ?? group.tasks[group.tasks.length - 1]?.id;
  }

  function bandsOf(group) {
    return reviewTimeline(group.tasks, group.reworks, anchorOf(group));
  }

  function select(taskId, focus) {
    dispatch('selectTask', { taskId, focus });
  }

  // The board already polls; re-fetching a plan on a status change rather than
  // on a timer keeps the graph current without a second clock.
  let signature = $derived(tasks.map(t => `${t.id}:${t.status}:${t.group_id}:${t.verdict}`).join('|'));
  let loadedFor = null;

  $effect(() => {
    const sig = signature;
    if (sig === loadedFor) return;
    loadedFor = sig;
    for (const group of grouped.groups) load(group.id);
  });

  async function load(groupId) {
    try {
      const plan = await fetchGroupPlan(groupId);
      plans = { ...plans, [groupId]: plan };
      errors = { ...errors, [groupId]: '' };
    } catch (e) {
      errors = { ...errors, [groupId]: e.message };
    }
  }

  // Worst-first, so a plan with one failed subtask is a failed plan however
  // many of its siblings finished.
  function rollup(plan, list) {
    const has = s => list.some(t => t.status === s);
    if (plan?.running || has('running')) return 'running';
    if (has('error')) return 'error';
    if (has('stopped')) return 'stopped';
    if (list.length && list.every(t => t.status === 'done')) return 'done';
    return 'idle';
  }

  async function run(groupId, fn) {
    busy = groupId;
    try {
      plans = { ...plans, [groupId]: await fn(groupId) };
      errors = { ...errors, [groupId]: '' };
      dispatch('refreshed');
    } catch (e) {
      errors = { ...errors, [groupId]: e.message };
    }
    busy = '';
  }
</script>

<div class="plan-view" id="plan-view" bind:this={root}>
  {#each grouped.groups as group (group.id)}
    {@const plan = plans[group.id]}
    {@const state = rollup(plan, group.tasks)}
    {@const review = rollupReview(group.tasks)}
    {@const bands = bandsOf(group)}
    <section class="plan">
      <div class="titleblock" data-map="head" data-map-state={state}>
        <div class="tb-idea">
          <div class="eyebrow">Idea</div>
          <p class="idea">{plan?.idea || group.tasks[0]?.title || 'Breakdown'}</p>
        </div>
        <dl class="tb-meta">
          <div class="tb-cell">
            <dt class="eyebrow">Waves</dt>
            <dd>{plan?.waves?.length ?? '—'}</dd>
          </div>
          <div class="tb-cell">
            <dt class="eyebrow">Subtasks</dt>
            <dd>{plan?.tasks?.length ?? group.tasks.length}</dd>
          </div>
          <div class="tb-cell">
            <dt class="eyebrow">Done</dt>
            <dd>{group.tasks.filter(t => t.status === 'done').length}</dd>
          </div>
          <div class="tb-cell">
            <dt class="eyebrow">State</dt>
            <dd class="state {state}"><span class="mark {state}"></span>{state}</dd>
          </div>
          {#if settings.review || review || group.reworks.length}
            <div class="tb-cell">
              <dt class="eyebrow">Review</dt>
              <dd>
                {#if review}
                  <ReviewBadge state={review} />
                {:else}
                  <span class="none">not yet</span>
                {/if}
              </dd>
            </div>
            <div class="tb-cell">
              <dt class="eyebrow">Rounds</dt>
              <dd>{group.reworks.length}</dd>
            </div>
          {/if}
        </dl>
      </div>

      <div class="plan-actions">
        {#if plan?.running}
          <button class="btn tiny" disabled={busy === group.id} onclick={() => run(group.id, stopGroup)}>Stop plan</button>
        {:else}
          <button class="btn tiny" disabled={busy === group.id} onclick={() => run(group.id, startGroup)}>
            {state === 'idle' ? 'Start plan' : 'Resume plan'}
          </button>
        {/if}
        <button
          class="btn tiny danger"
          disabled={busy === group.id}
          onclick={() => dispatch('deleteGroup', {
            groupId: group.id,
            title: plan?.idea || 'this plan',
            count: plan?.tasks?.length ?? group.tasks.length,
          })}
        >Delete plan</button>
      </div>

      {#if errors[group.id]}
        <div class="notice bad">{errors[group.id]}</div>
      {/if}

      {#if plan}
        <PlanGraph
          plan={plan}
          {bands}
          {selectedId}
          on:selectTask={(e) => dispatch('selectTask', e.detail)}
        />
      {:else}
        <div class="pending eyebrow">Resolving plan…</div>
      {/if}
    </section>
  {/each}

  {#if grouped.singles.length}
    <section class="singles">
      <h2 class="eyebrow section-rule">Not split · {grouped.singles.length}</h2>
      <div class="single-list">
        {#each grouped.singles as entry (entry.task.id)}
          {@const task = entry.task}
          <div class="strand">
            <button
              class="single {task.status}"
              class:selected={selectedId === task.id}
              data-map="row"
              data-map-state={task.status}
              onclick={() => select(task.id)}
            >
              <span class="mark {task.status}"></span>
              <span class="single-title">{task.title}</span>
              <span class="single-col eyebrow">{task.column}</span>
              <span class="state {task.status}">{task.status}</span>
            </button>

            <!-- The same sequence the graph draws, in the one dimension a list
                 has. A task never split has no waves to hang it under. -->
            {#each reviewTimeline([task], entry.reworks, task.id) as band (band.key)}
              {#if band.kind === 'verdict'}
                <button
                  class="band verdict {band.state.tone}"
                  data-map="row"
                  data-map-state="review"
                  title="Open the review agent's own trace"
                  onclick={() => select(band.traceId, 'review')}
                >
                  <span class="mark {band.state.key}"></span>
                  <span class="band-label">{band.state.label}</span>
                  <span class="band-note">{band.note}</span>
                  <span class="band-open eyebrow">trace →</span>
                </button>
              {:else}
                <button
                  class="band rework {band.task.status}"
                  class:selected={selectedId === band.task.id}
                  data-map="row"
                  data-map-state={band.task.status}
                  onclick={() => select(band.task.id)}
                >
                  <span class="mark {band.task.status}"></span>
                  <span class="band-label">Rework {band.task.review_round}</span>
                  <span class="band-note">{band.task.title}</span>
                  <span class="state {band.task.status}">{band.task.status}</span>
                </button>
              {/if}
            {/each}
          </div>
        {/each}
      </div>
    </section>
  {/if}

  {#if grouped.groups.length === 0 && grouped.singles.length === 0}
    <div class="blank">
      <p class="idea">Nothing planned yet.</p>
      <p>Break down an idea and its subtasks appear here as a graph — one node per
         file owner, one edge per path something else needs.</p>
    </div>
  {/if}
</div>

<PlanMinimap container={root} {signature} />

<style>
  .plan-view {
    padding: 24px 28px 64px;
    display: flex;
    flex-direction: column;
    gap: 44px;
  }

  /* The minimap is fixed over the right edge, so the plan keeps clear of it. */
  @media (min-width: 721px) {
    .plan-view { padding-right: 94px; }
  }

  /* The title block borrows an engineering drawing's: what a person asked for
     on the left, what the machine worked out in ruled cells on the right. */
  .titleblock {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    border: 1px solid var(--rule);
    background: var(--panel);
  }
  .tb-idea { padding: 16px 20px 18px; min-width: 0; }
  .tb-idea .idea {
    margin: 8px 0 0;
    font-size: clamp(20px, 2.6vw, 28px);
  }
  .tb-meta {
    display: grid;
    grid-template-columns: repeat(2, minmax(88px, auto));
    margin: 0;
    border-left: 1px solid var(--rule);
  }
  .tb-cell { padding: 9px 14px; border-bottom: 1px solid var(--rule); }
  .tb-cell:nth-child(odd) { border-right: 1px solid var(--rule); }
  .tb-cell:nth-last-child(-n+2) { border-bottom: none; }
  .tb-cell dt { margin: 0 0 3px; }
  .tb-cell dd { margin: 0; font-family: var(--f-mono); font-size: 14px; }

  .plan-actions { display: flex; gap: 6px; margin: 10px 0 4px; }

  .pending { padding: 20px 0; }

  .section-rule {
    display: flex;
    align-items: center;
    gap: 12px;
    margin: 0 0 12px;
    font-weight: 400;
  }
  .section-rule::after { content: ''; flex: 1; height: 1px; background: var(--rule); }

  .tb-cell .none { font-family: var(--f-mono); font-size: 11px; color: var(--ink-3); }

  .single-list { display: flex; flex-direction: column; gap: 6px; max-width: 780px; }
  .strand { display: flex; flex-direction: column; gap: 4px; }
  .single {
    display: flex;
    align-items: center;
    gap: 12px;
    width: 100%;
    padding: 9px 12px;
    background: var(--panel);
    border: 1px solid var(--rule);
    border-radius: 0;
    font: inherit;
    color: inherit;
    text-align: left;
    cursor: pointer;
    transition: border-color .12s;
  }
  .single:hover { border-color: var(--ink); }
  .single.selected { box-shadow: inset 0 0 0 1px var(--ink); border-color: var(--ink); }
  .single.running { border-color: var(--live); box-shadow: inset 3px 0 0 var(--live); }
  .single-title { flex: 1; min-width: 0; font-size: 13.5px; font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .single-col { flex: none; }

  /* Indented under the row it belongs to, and ruled into it on the left, so the
     sequence reads as one strand rather than as three unrelated rows. */
  .band {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-left: 22px;
    width: calc(100% - 22px);
    padding: 7px 12px;
    background: var(--panel);
    border: 1px solid var(--rule);
    border-left: 2px solid var(--judge);
    border-radius: 0;
    font: inherit;
    color: inherit;
    text-align: left;
    cursor: pointer;
    transition: border-color .12s;
  }
  .band:hover { border-color: var(--ink); border-left-color: var(--judge); }
  .band.selected { box-shadow: inset 0 0 0 1px var(--ink); }
  .band.verdict { background: var(--judge-wash); }
  .band.verdict.fault { background: var(--fault-wash); border-left-color: var(--fault); }
  .band.verdict.done { background: var(--panel); }
  .band.rework.running { border-color: var(--live); border-left-color: var(--live); }

  .band-label {
    flex: none;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .14em;
    text-transform: uppercase;
    color: var(--judge);
  }
  .band.verdict.fault .band-label { color: var(--fault); }
  .band.verdict.done .band-label { color: var(--ink-2); }
  .band-note {
    flex: 1;
    min-width: 0;
    font-size: 12.5px;
    color: var(--ink-2);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .band-open { flex: none; }
  .band:hover .band-open { color: var(--ink); }

  .blank { max-width: 46ch; padding: 60px 0; }
  .blank .idea { margin: 0 0 10px; font-size: 26px; }
  .blank p { margin: 0; color: var(--ink-2); line-height: 1.6; }

  @media (max-width: 720px) {
    .plan-view { padding: 18px 16px 48px; }
    .titleblock { grid-template-columns: 1fr; }
    .tb-meta { border-left: none; border-top: 1px solid var(--rule); }
  }
</style>
