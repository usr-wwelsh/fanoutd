<script>
  import Column from './Column.svelte';
  import { createEventDispatcher } from 'svelte';

  let { tasks, selectedId } = $props();

  const dispatch = createEventDispatcher();

  let columns = ['ideas', 'todo', 'review', 'finished'];
  let columnLabels = { ideas: 'Ideas', todo: 'To-Do', review: 'Review', finished: 'Finished' };

  function getTasks(col) {
    return tasks.filter(t => t.column === col);
  }

  function onSelectTask(e) {
    dispatch('selectTask', e.detail);
  }

  function onTaskMoved(e) {
    dispatch('taskMoved', e.detail);
  }

  function onDeleteTask(e) {
    dispatch('deleteTask', e.detail);
  }

  function onGroupMoved(e) {
    dispatch('groupMoved', e.detail);
  }

  function onDeleteGroup(e) {
    dispatch('deleteGroup', e.detail);
  }

  function onRefreshed() {
    dispatch('refreshed');
  }
</script>

<div class="board">
  {#each columns as col}
    <Column
      col={col}
      label={columnLabels[col]}
      tasks={getTasks(col)}
      all={tasks}
      selectedId={selectedId}
      on:selectTask={onSelectTask}
      on:taskMoved={onTaskMoved}
      on:deleteTask={onDeleteTask}
      on:groupMoved={onGroupMoved}
      on:deleteGroup={onDeleteGroup}
      on:refreshed={onRefreshed}
    />
  {/each}
</div>

<style>
  .board {
    display: flex;
    gap: 20px;
    padding: 24px 28px 64px;
    align-items: flex-start;
    overflow-x: auto;
    min-height: calc(100vh - 64px);
  }

  @media (max-width: 720px) {
    .board { padding: 18px 16px 48px; gap: 14px; }
  }
</style>
