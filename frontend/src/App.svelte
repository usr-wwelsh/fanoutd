<script>
  import Board from './lib/components/Board.svelte';
  import PlanView from './lib/components/PlanView.svelte';
  import TaskDetail from './lib/components/TaskDetail.svelte';
  import NewTaskModal from './lib/components/NewTaskModal.svelte';
  import BreakdownModal from './lib/components/BreakdownModal.svelte';
  import SettingsModal from './lib/components/SettingsModal.svelte';
  import Login from './lib/components/Login.svelte';
  import { onMount } from 'svelte';
  import { active, toggleTheme } from './lib/theme.svelte.js';
  import { loadSettings } from './lib/config.svelte.js';
  import { reviewState } from './lib/review.js';
  import { AuthError, fetchAuthStatus, fetchTasks, logout, moveTask, deleteTask, moveGroup, deleteGroup } from './lib/api.js';

  let tasks = $state([]);
  let selectedId = $state(null);
  // What the detail panel was opened to answer, cleared as soon as something
  // else is opened. Only a verdict sets it, and only the trace reads it.
  let focus = $state(null);
  let showNewTask = $state(false);
  let showBreakdown = $state(false);
  let showSettings = $state(false);
  let loading = $state(true);
  let error = $state('');
  let authRequired = $state(false);
  let authed = $state(false);
  let checkingAuth = $state(true);
  let poll = null;

  // The plan is the view this tool is actually about, so it opens there; the
  // board is where you file things once they exist.
  let view = $state(localStorage.getItem('fanoutd.view') === 'board' ? 'board' : 'plan');

  function setView(next) {
    view = next;
    localStorage.setItem('fanoutd.view', next);
  }

  let running = $derived(tasks.filter(t => t.status === 'running').length);
  // Work nobody has answered for yet. It is the one count that would otherwise
  // go unnoticed: nothing is running, nothing has failed, and the board looks
  // idle while several finished runs wait on a verdict.
  let held = $derived(tasks.filter(t => reviewState(t)?.tone === 'judge').length);
  let dark = $derived(active() === 'dark');

  onMount(() => {
    init();
    return stopPolling;
  });

  async function init() {
    try {
      const status = await fetchAuthStatus();
      authRequired = status.required;
      authed = status.authenticated;
    } catch (e) {
      // The auth endpoint is unauthenticated, so a failure here means the
      // server is unreachable rather than locked. Show the board and let the
      // task load report the real problem.
      authed = true;
      error = e.message;
    }
    checkingAuth = false;
    if (authed) startBoard();
  }

  function startBoard() {
    stopPolling();
    loading = true;
    // Settings come from the environment the server started in, so they are
    // fetched once here rather than with every poll.
    loadSettings();
    loadTasks();
    poll = setInterval(loadTasks, 2000);
  }

  function stopPolling() {
    if (poll) clearInterval(poll);
    poll = null;
  }

  // A 401 anywhere means the session expired; drop back to the login prompt
  // rather than flashing an error banner every two seconds.
  function handleUnauthenticated() {
    stopPolling();
    authRequired = true;
    authed = false;
    selectedId = null;
    focus = null;
    showNewTask = false;
    showBreakdown = false;
    showSettings = false;
    error = '';
  }

  async function handleLogout() {
    try {
      await logout();
    } catch (e) {
      // Losing the cookie locally is the point; a failed call still logs out.
    }
    handleUnauthenticated();
  }

  async function loadTasks() {
    try {
      tasks = await fetchTasks();
      error = '';
    } catch (e) {
      if (e instanceof AuthError) {
        handleUnauthenticated();
      } else {
        error = e.message;
        console.error('Failed to load tasks', e);
      }
    }
    loading = false;
  }

  function handleRefresh() {
    loadTasks();
  }

  // Clicking the same card again closes the panel, but a click carrying a
  // question does not: arriving at a task from its verdict has to open it on the
  // review, whether or not that task was already the one on screen.
  function handleSelectTask(taskId, next = null) {
    if (next) {
      selectedId = taskId;
      focus = next;
      return;
    }
    focus = null;
    selectedId = selectedId === taskId ? null : taskId;
  }

  // Deleting removes the task, its trace, and its output files, so it asks first.
  async function handleDeleteTask({ taskId, title }) {
    const task = tasks.find(t => t.id === taskId);
    const shared = task && tasks.some(t => t.id !== taskId && t.workspace_id === task.workspace_id);
    const files = shared ? 'Its workspace is shared, so the files stay.' : 'Its output files are deleted too.';
    if (!confirm(`Delete "${title ?? task?.title ?? taskId}"?\n\n${files}`)) return;
    try {
      await deleteTask(taskId);
      if (selectedId === taskId) selectedId = null;
      error = '';
    } catch (e) {
      if (reportError(e)) return;
    }
    loadTasks();
  }

  // A plan is one card, so deleting it is one confirm for the whole thing —
  // subtasks share a workspace, and the files go with the last of them anyway.
  async function handleDeleteGroup({ groupId, title, count }) {
    if (!confirm(`Delete "${title}"?\n\nAll ${count} subtasks and their shared workspace are deleted.`)) return;
    try {
      await deleteGroup(groupId);
      if (tasks.some(t => t.id === selectedId && t.group_id === groupId)) selectedId = null;
      error = '';
    } catch (e) {
      if (reportError(e)) return;
    }
    loadTasks();
  }

  async function handleGroupMoved({ groupId, column }) {
    try {
      await moveGroup(groupId, column);
      error = '';
    } catch (e) {
      if (reportError(e)) return;
    }
    loadTasks();
  }

  async function handleTaskMoved({ taskId, column }) {
    try {
      await moveTask(taskId, column);
      error = '';
    } catch (e) {
      if (reportError(e)) return;
    }
    loadTasks();
  }

  // Returns true when the error was a lost session, which is handled by
  // returning to the login prompt rather than by showing a banner.
  function reportError(e) {
    if (e instanceof AuthError) {
      handleUnauthenticated();
      return true;
    }
    error = e.message;
    return false;
  }
</script>

{#if checkingAuth}
  <div class="loading eyebrow">Checking session…</div>
{:else if !authed}
  <Login onAuthenticated={() => { authed = true; startBoard(); }} />
{:else}

<main>
  <header>
    <div class="brand">
      <h1>fanout<span>d</span></h1>
      {#if running > 0}
        <span class="state running"><span class="mark running"></span>{running} running</span>
      {/if}
      {#if held > 0}
        <span class="held"><span class="mark awaiting"></span>{held} in review</span>
      {/if}
    </div>

    <div class="modes" role="group" aria-label="View">
      <button class="mode" aria-pressed={view === 'plan'} onclick={() => setView('plan')}>Plan</button>
      <button class="mode" aria-pressed={view === 'board'} onclick={() => setView('board')}>Board</button>
    </div>

    <div class="header-actions">
      <button
        class="btn quiet theme"
        onclick={toggleTheme}
        title="Switch to {dark ? 'light' : 'dark'} for this tab — reopening follows your system"
        aria-label="Switch to {dark ? 'light' : 'dark'} theme"
      >{dark ? '☀' : '☾'}</button>
      <!-- Drawn rather than typed: the gear codepoint renders as a flat glyph in
           most UI fonts and reads as a snowflake at this size. -->
      <button
        class="btn quiet icon"
        onclick={() => showSettings = true}
        title="Server settings"
        aria-label="Server settings"
      >
        <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor"
             stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <circle cx="12" cy="12" r="3" />
          <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.6 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.6a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9v.09a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
        </svg>
      </button>
      <button class="btn quiet" onclick={handleRefresh} disabled={loading}>Refresh</button>
      <button class="btn" onclick={() => showNewTask = true}>New task</button>
      <button class="btn primary" onclick={() => showBreakdown = true}>Break down an idea</button>
      {#if authRequired}
        <button class="btn quiet" onclick={handleLogout}>Lock</button>
      {/if}
    </div>
  </header>

  {#if error}
    <div class="notice bad banner">{error}</div>
  {/if}

  {#if loading}
    <div class="loading eyebrow">Loading tasks…</div>
  {:else if view === 'plan'}
    <PlanView
      tasks={tasks}
      selectedId={selectedId}
      on:selectTask={(e) => handleSelectTask(e.detail.taskId, e.detail.focus)}
      on:deleteGroup={(e) => handleDeleteGroup(e.detail)}
      on:refreshed={() => loadTasks()}
    />
  {:else}
    <Board
      tasks={tasks}
      selectedId={selectedId}
      on:selectTask={(e) => handleSelectTask(e.detail.taskId, e.detail.focus)}
      on:taskMoved={(e) => handleTaskMoved(e.detail)}
      on:deleteTask={(e) => handleDeleteTask(e.detail)}
      on:groupMoved={(e) => handleGroupMoved(e.detail)}
      on:deleteGroup={(e) => handleDeleteGroup(e.detail)}
      on:refreshed={() => loadTasks()}
    />
  {/if}

  {#if selectedId !== null}
    {@const task = tasks.find(t => t.id === selectedId)}
    {#if task}
      <div class="detail-panel">
        <button class="close-btn" aria-label="Close detail" onclick={() => selectedId = null}>✕</button>
        <TaskDetail
          task={task}
          tasks={tasks}
          {focus}
          on:refreshed={() => loadTasks()}
          on:openTask={(e) => { focus = null; selectedId = e.detail; loadTasks(); }}
          on:openReview={(e) => { focus = 'review'; selectedId = e.detail; loadTasks(); }}
          on:deleteTask={(e) => handleDeleteTask(e.detail)}
        />
      </div>
    {/if}
  {/if}

  {#if showNewTask}
    <NewTaskModal
      on:close={() => showNewTask = false}
      on:created={() => { showNewTask = false; loadTasks(); }}
    />
  {/if}

  {#if showSettings}
    <SettingsModal on:close={() => showSettings = false} on:saved={() => loadTasks()} />
  {/if}

  <!-- Unlike New Task, this stays open after it submits: the wave plan and the
       subtasks running under it are the result, and closing would hide them. -->
  {#if showBreakdown}
    <BreakdownModal
      on:close={() => showBreakdown = false}
      on:created={() => loadTasks()}
      on:openTask={(e) => { focus = null; selectedId = e.detail; loadTasks(); }}
    />
  {/if}
</main>
{/if}

<style>
  main { min-height: 100vh; }

  header {
    display: flex;
    align-items: center;
    gap: 20px;
    flex-wrap: wrap;
    padding: 14px 28px;
    background: var(--panel);
    border-bottom: 1px solid var(--rule);
  }

  .brand { display: flex; align-items: baseline; gap: 14px; }
  .brand h1 {
    margin: 0;
    font-family: var(--f-mono);
    font-size: 17px;
    font-weight: 400;
    letter-spacing: .16em;
  }
  .brand h1 span { color: var(--ink-3); }
  .held {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .15em;
    text-transform: uppercase;
    color: var(--judge);
  }

  .modes { display: flex; border: 1px solid var(--rule); border-radius: var(--r); overflow: hidden; }
  .mode {
    font-family: var(--f-mono);
    font-size: 10.5px;
    letter-spacing: .15em;
    text-transform: uppercase;
    padding: 6px 15px;
    border: none;
    background: none;
    color: var(--ink-3);
    cursor: pointer;
  }
  .mode + .mode { border-left: 1px solid var(--rule); }
  .mode[aria-pressed="true"] { background: var(--ink); color: var(--panel); }
  .mode:focus-visible { outline-offset: -2px; }

  .header-actions { display: flex; gap: 6px; margin-left: auto; flex-wrap: wrap; }

  .theme { font-size: 14px; line-height: 1; padding: 6px 9px; }

  .icon {
    display: inline-flex;
    align-items: center;
    padding: 5px 8px;
    color: var(--ink-2);
  }
  .icon:hover { color: var(--ink); }

  .banner { margin: 14px 28px 0; }

  .loading {
    display: flex;
    justify-content: center;
    align-items: center;
    height: 55vh;
  }

  .detail-panel {
    position: fixed;
    right: 0;
    top: 0;
    bottom: 0;
    width: 480px;
    max-width: 92vw;
    background: var(--panel);
    border-left: 1px solid var(--ink);
    overflow-y: auto;
    padding: 22px 24px 40px;
    box-shadow: var(--shadow-side);
    z-index: 20;
  }
  .close-btn {
    position: absolute;
    top: 14px;
    right: 16px;
    background: none;
    border: none;
    color: var(--ink-3);
    font-size: 14px;
    cursor: pointer;
    padding: 4px;
  }
  .close-btn:hover { color: var(--ink); }

  @media (max-width: 720px) {
    header { padding: 12px 16px; gap: 12px; }
    .header-actions { margin-left: 0; width: 100%; }
    .banner { margin: 12px 16px 0; }
  }
</style>
