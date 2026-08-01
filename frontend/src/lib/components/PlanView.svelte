<script>
  // The plan view is the board turned on its side: every breakdown as the graph
  // it actually is, with the idea it came from above it. Tasks that were never
  // split have no graph to draw, so they sit underneath as a plain list.
  import { createEventDispatcher } from 'svelte';
  import { fetchGroupPlan, startGroup, stopGroup } from '../api.js';
  import PlanGraph from './PlanGraph.svelte';
  import PlanMinimap from './PlanMinimap.svelte';

  let { tasks, selectedId } = $props();

  const dispatch = createEventDispatcher();

  let plans = $state({});
  let errors = $state({});
  let busy = $state('');
  let root = $state(null);

  let grouped = $derived.by(() => {
    const map = new Map();
    const singles = [];
    for (const task of tasks) {
      if (!task.group_id) { singles.push(task); continue; }
      let group = map.get(task.group_id);
      if (!group) { group = { id: task.group_id, tasks: [] }; map.set(task.group_id, group); }
      group.tasks.push(task);
    }
    return { groups: [...map.values()], singles };
  });

  // The board already polls; re-fetching a plan on a status change rather than
  // on a timer keeps the graph current without a second clock.
  let signature = $derived(tasks.map(t => `${t.id}:${t.status}:${t.group_id}`).join('|'));
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
        {#each grouped.singles as task (task.id)}
          <button
            class="single {task.status}"
            class:selected={selectedId === task.id}
            data-map="row"
            data-map-state={task.status}
            onclick={() => dispatch('selectTask', { taskId: task.id })}
          >
            <span class="mark {task.status}"></span>
            <span class="single-title">{task.title}</span>
            <span class="single-col eyebrow">{task.column}</span>
            <span class="state {task.status}">{task.status}</span>
          </button>
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

  .single-list { display: flex; flex-direction: column; gap: 6px; max-width: 780px; }
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

  .blank { max-width: 46ch; padding: 60px 0; }
  .blank .idea { margin: 0 0 10px; font-size: 26px; }
  .blank p { margin: 0; color: var(--ink-2); line-height: 1.6; }

  @media (max-width: 720px) {
    .plan-view { padding: 18px 16px 48px; }
    .titleblock { grid-template-columns: 1fr; }
    .tb-meta { border-left: none; border-top: 1px solid var(--rule); }
  }
</style>
