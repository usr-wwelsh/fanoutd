<script>
  import TaskCard from './TaskCard.svelte';
  import GroupCard from './GroupCard.svelte';
  import { createEventDispatcher } from 'svelte';

  let { col, label, tasks, selectedId } = $props();

  // An empty column should say what to do with it rather than name itself again.
  const prompts = {
    ideas: 'Nothing waiting. New task or Break down an idea starts one here.',
    todo: 'Drop a task here to start its agent.',
    finished: 'Finished work lands here on its own.',
  };

  const dispatch = createEventDispatcher();

  let dragOver = $state(false);

  // Subtasks of one breakdown collapse into a single card, placed where the
  // first of them would have been. Everything else is an ordinary task card.
  let items = $derived.by(() => {
    const out = [];
    const groups = new Map();
    for (const task of tasks) {
      if (!task.group_id) {
        out.push({ kind: 'task', key: task.id, task });
        continue;
      }
      let group = groups.get(task.group_id);
      if (!group) {
        group = { kind: 'group', key: `g:${task.group_id}`, groupId: task.group_id, tasks: [] };
        groups.set(task.group_id, group);
        out.push(group);
      }
      group.tasks.push(task);
    }
    return out;
  });

  // The count is what the column holds, which is cards rather than tasks once a
  // plan is one card.
  let count = $derived(items.length);

  function handleDragOver(e) {
    e.preventDefault();
    dragOver = true;
  }

  function handleDragLeave() {
    dragOver = false;
  }

  // A group header drags as `group:<id>`; a task card drags as its bare id.
  function handleDrop(e) {
    e.preventDefault();
    dragOver = false;
    const payload = e.dataTransfer.getData('text/plain');
    if (!payload) return;
    if (payload.startsWith('group:')) {
      dispatch('groupMoved', { groupId: payload.slice(6), column: col });
    } else {
      dispatch('taskMoved', { taskId: payload, column: col });
    }
  }

  function handleSelect(taskId) {
    dispatch('selectTask', { taskId });
  }

  function handleDelete(task) {
    dispatch('deleteTask', { taskId: task.id, title: task.title });
  }
</script>

<div class="column" class:drag-over={dragOver} role="list" aria-label="{label} column">
  <div class="column-header">
    <h2 class="eyebrow">{label}</h2>
    <span class="count">{count}</span>
  </div>
  <div class="column-body" ondragover={handleDragOver} ondragleave={handleDragLeave} ondrop={handleDrop} role="list">
    {#each items as item (item.key)}
      {#if item.kind === 'group'}
        <GroupCard
          groupId={item.groupId}
          tasks={item.tasks}
          {selectedId}
          on:selectTask={(e) => handleSelect(e.detail.taskId)}
          on:deleteTask={(e) => dispatch('deleteTask', e.detail)}
          on:deleteGroup={(e) => dispatch('deleteGroup', e.detail)}
          on:refreshed={() => dispatch('refreshed')}
        />
      {:else}
        <TaskCard
          task={item.task}
          isSelected={selectedId === item.task.id}
          on:select={() => handleSelect(item.task.id)}
          on:delete={() => handleDelete(item.task)}
        />
      {/if}
    {/each}
    {#if items.length === 0}
      <div class="empty">{prompts[col]}</div>
    {/if}
  </div>
</div>

<style>
  .column {
    flex: 1;
    min-width: 268px;
    max-width: 360px;
    display: flex;
    flex-direction: column;
    border: 1px solid transparent;
    transition: border-color .12s, background .12s;
  }
  .column.drag-over {
    border-color: var(--live);
    background: var(--live-wash);
  }
  .column-header {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    padding: 0 2px 8px;
    border-bottom: 1px solid var(--ink);
  }
  .column-header h2 { margin: 0; font-weight: 400; color: var(--ink); }
  .count { font-family: var(--f-mono); font-size: 12px; color: var(--ink-3); }
  .column-body {
    flex: 1;
    padding: 10px 2px 2px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-height: 140px;
  }
  .empty {
    border: 1px dashed var(--rule);
    padding: 22px 14px;
    font-size: 12.5px;
    line-height: 1.5;
    color: var(--ink-3);
  }
</style>
