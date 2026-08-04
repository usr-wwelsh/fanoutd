<script>
  // Two agents write into one trace. The author's steps are the run; the
  // reviewer's are a second pass over the finished work by something that never
  // saw the run, and they are kept on the same task on purpose — the verdict
  // belongs where the work is. So they are told apart here rather than filed
  // apart: one lane, marked, with a filter for reading either on its own.
  import { fetchTrace } from '../api.js';
  import { isReviewStep, stepAction, verdictStep } from '../review.js';

  let { taskId, live = false, focus = null } = $props();

  let trace = $state([]);
  let expanded = $state(true);
  let filter = $state('all');

  // Arriving from a verdict opens the trace on the reviewer's half, which is the
  // question that was asked. Keyed on the task so that opening a second one from
  // the same verdict asks it again rather than inheriting the answer.
  let focusedFor = null;
  $effect(() => {
    const key = `${taskId}:${focus}`;
    if (key === focusedFor) return;
    focusedFor = key;
    if (focus === 'review') {
      filter = 'review';
      expanded = true;
    }
  });

  $effect(() => {
    const id = taskId;
    const interval = live ? 2000 : 10000;
    let cancelled = false;

    const load = async () => {
      try {
        const steps = await fetchTrace(id);
        if (!cancelled) trace = steps;
      } catch (e) {
        console.error('Failed to load trace', e);
      }
    };

    load();
    const timer = setInterval(load, interval);
    return () => { cancelled = true; clearInterval(timer); };
  });

  let reviewCount = $derived(trace.filter(isReviewStep).length);
  let authorCount = $derived(trace.length - reviewCount);
  let shown = $derived(
    filter === 'all' ? trace
      : filter === 'review' ? trace.filter(isReviewStep)
      : trace.filter(s => !isReviewStep(s))
  );

  function toggle() {
    expanded = !expanded;
  }
</script>

<div class="trace-container">
  <button class="trace-toggle" onclick={toggle} aria-expanded={expanded}>
    <span class="caret" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
    Trace
    <span class="count">
      {trace.length} step{trace.length === 1 ? '' : 's'}{#if reviewCount} · {reviewCount} review{/if}
    </span>
  </button>

  {#if expanded}
    <!-- Only worth offering once there are two kinds of step to separate. -->
    {#if reviewCount > 0}
      <div class="lanes" role="group" aria-label="Whose steps to show">
        <button class="lane" aria-pressed={filter === 'all'} onclick={() => filter = 'all'}>All</button>
        <button class="lane" aria-pressed={filter === 'author'} onclick={() => filter = 'author'}>Agent · {authorCount}</button>
        <button class="lane judge" aria-pressed={filter === 'review'} onclick={() => filter = 'review'}>Review · {reviewCount}</button>
      </div>
    {/if}

    {#if trace.length === 0}
      <div class="no-trace">Nothing recorded yet. Steps appear here as the agent works.</div>
    {:else if shown.length === 0}
      <div class="no-trace">No steps from this half of the trace.</div>
    {:else}
      {#each shown as step (step.id)}
        {@const review = isReviewStep(step)}
        {@const verdict = verdictStep(step)}
        <div class="trace-step" class:review class:verdict={!!verdict}>
          <div class="step-header">
            <span class="step-number">{String(step.step_number).padStart(2, '0')}</span>
            {#if review}<span class="who">reviewer</span>{/if}
            <span class="step-time">{new Date(step.timestamp).toLocaleTimeString()}</span>
          </div>
          <div class="step-action" class:verdict-action={!!verdict}>
            {#if verdict}<span class="mark {verdict.key}"></span>{/if}{stepAction(step)}
          </div>
          {#if step.tool_name}
            <div class="step-tool">
              <span class="tool-name">{step.tool_name}</span>
              <pre>{step.tool_result}</pre>
            </div>
          {:else if step.tool_result}
            <div class="step-tool note"><pre>{step.tool_result}</pre></div>
          {/if}
          {#if step.response}
            <details class="step-response">
              <summary>Raw model response</summary>
              <pre>{step.response}</pre>
            </details>
          {/if}
        </div>
      {/each}
    {/if}
  {/if}
</div>

<style>
  .trace-container { margin-top: 18px; }

  .trace-toggle {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 7px 0;
    background: none;
    border: none;
    border-top: 1px solid var(--ink);
    color: var(--ink);
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .18em;
    text-transform: uppercase;
    cursor: pointer;
  }
  .caret { color: var(--ink-3); letter-spacing: 0; }
  .trace-toggle .count { margin-left: auto; color: var(--ink-3); }

  .lanes { display: flex; gap: 0; margin: 6px 0 4px; border: 1px solid var(--rule); width: fit-content; }
  .lane {
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .12em;
    text-transform: uppercase;
    padding: 4px 10px;
    border: none;
    background: none;
    color: var(--ink-3);
    cursor: pointer;
  }
  .lane + .lane { border-left: 1px solid var(--rule); }
  .lane[aria-pressed="true"] { background: var(--ink); color: var(--panel); }
  .lane.judge[aria-pressed="true"] { background: var(--judge); color: var(--on-judge); }

  .no-trace {
    font-size: 12px;
    color: var(--ink-3);
    padding: 10px 0;
    line-height: 1.5;
  }

  .trace-step {
    border-bottom: 1px solid var(--rule-soft);
    padding: 10px 0;
    font-size: 12.5px;
  }
  /* The reviewer's steps are inset behind its own rule, so a scroll through a
     long trace shows at a glance where the second agent took over. */
  .trace-step.review {
    border-left: 2px solid var(--judge);
    padding-left: 10px;
    background: var(--judge-wash);
  }
  .trace-step.review.verdict { border-left-width: 3px; }

  .step-header {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 8px;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .12em;
    margin-bottom: 4px;
  }
  .step-number { color: var(--ink); }
  .who { color: var(--judge); text-transform: uppercase; letter-spacing: .16em; }
  .step-time { color: var(--ink-3); margin-left: auto; }
  .step-action { margin-bottom: 7px; line-height: 1.5; color: var(--ink-2); }
  .verdict-action {
    display: flex;
    align-items: center;
    gap: 7px;
    font-family: var(--f-mono);
    font-size: 11px;
    letter-spacing: .14em;
    text-transform: uppercase;
    color: var(--ink);
  }

  .tool-name {
    display: inline-block;
    font-family: var(--f-mono);
    font-size: 10px;
    padding: 1px 6px;
    background: var(--ink);
    color: var(--panel);
    margin-bottom: 5px;
  }
  .trace-step.review .tool-name { background: var(--judge); color: var(--on-judge); }
  .step-tool.note pre { color: var(--ink-2); font-style: italic; }

  .step-tool pre, .step-response pre {
    white-space: pre-wrap;
    word-break: break-word;
    max-height: 240px;
    overflow-y: auto;
    background: var(--sunk);
    border: 1px solid var(--rule-soft);
    padding: 8px 9px;
    font-family: var(--f-mono);
    font-size: 11px;
    line-height: 1.55;
    margin: 0;
  }
  .trace-step.review .step-tool pre, .trace-step.review .step-response pre {
    background: var(--panel);
  }
  .step-response { margin-top: 6px; }
  .step-response summary {
    cursor: pointer;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .14em;
    text-transform: uppercase;
    color: var(--ink-3);
    margin-bottom: 5px;
  }
  .step-response summary:hover { color: var(--ink); }
</style>
