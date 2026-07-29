<script>
  import { fetchTrace } from '../api.js';

  let { taskId, live = false } = $props();

  let trace = $state([]);
  let expanded = $state(true);

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

  function toggle() {
    expanded = !expanded;
  }
</script>

<div class="trace-container">
  <button class="trace-toggle" onclick={toggle} aria-expanded={expanded}>
    <span class="caret" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
    Trace
    <span class="count">{trace.length} step{trace.length === 1 ? '' : 's'}</span>
  </button>
  {#if expanded}
    {#if trace.length === 0}
      <div class="no-trace">Nothing recorded yet. Steps appear here as the agent works.</div>
    {:else}
      {#each trace as step (step.id)}
        <div class="trace-step">
          <div class="step-header">
            <span class="step-number">{String(step.step_number).padStart(2, '0')}</span>
            <span class="step-time">{new Date(step.timestamp).toLocaleTimeString()}</span>
          </div>
          <div class="step-action">{step.action}</div>
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
  .step-header {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .12em;
    margin-bottom: 4px;
  }
  .step-number { color: var(--ink); }
  .step-time { color: var(--ink-3); }
  .step-action { margin-bottom: 7px; line-height: 1.5; color: var(--ink-2); }

  .tool-name {
    display: inline-block;
    font-family: var(--f-mono);
    font-size: 10px;
    padding: 1px 6px;
    background: var(--ink);
    color: var(--panel);
    margin-bottom: 5px;
  }
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
