<script>
  import { createEventDispatcher } from 'svelte';

  let { task, isSelected } = $props();

  const dispatch = createEventDispatcher();

  const labels = {
    idle: 'Idle',
    running: 'Running',
    done: 'Done',
    stopped: 'Stopped',
    error: 'Error',
  };

  let status = $derived(labels[task.status] ? task.status : 'idle');

  function handleClick() {
    dispatch('select');
  }

  function handleKeydown(e) {
    if (e.key === 'Enter') handleClick();
  }
</script>

<div
  class="task-card {status}"
  class:selected={isSelected}
  role="button"
  tabindex="0"
  aria-label={task.title}
  draggable="true"
  ondragstart={(e) => e.dataTransfer.setData('text/plain', task.id)}
  onclick={handleClick}
  onkeydown={handleKeydown}
>
  <div class="task-title">
    <span>{task.title}</span>
    <button
      class="x-btn"
      title="Delete this task"
      aria-label="Delete {task.title}"
      onclick={(e) => { e.stopPropagation(); dispatch('delete'); }}
    >✕</button>
  </div>
  {#if task.goal}
    <div class="task-goal">{task.goal}</div>
  {/if}
  <div class="state {status}"><span class="mark {status}"></span>{labels[status]}</div>
</div>

<style>
  .task-card {
    background: var(--panel);
    border: 1px solid var(--rule);
    padding: 10px 12px;
    cursor: grab;
    transition: border-color .12s, box-shadow .12s;
  }
  .task-card:hover { border-color: var(--ink); }
  .task-card.selected { border-color: var(--ink); box-shadow: inset 0 0 0 1px var(--ink); }
  .task-card.running { border-color: var(--live); box-shadow: inset 3px 0 0 var(--live); }
  .task-card.error { border-color: var(--fault); box-shadow: inset 3px 0 0 var(--fault); }
  .task-card.idle { background: var(--sunk); }

  .task-title {
    display: flex;
    align-items: start;
    gap: 8px;
    font-size: 13.5px;
    font-weight: 600;
    line-height: 1.3;
    margin-bottom: 3px;
  }
  .task-title span { flex: 1; min-width: 0; }
  .task-card:hover .x-btn { opacity: 1; }

  .task-goal {
    font-size: 12px;
    line-height: 1.45;
    color: var(--ink-2);
    margin-bottom: 8px;
    display: -webkit-box;
    -webkit-line-clamp: 3;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
</style>
