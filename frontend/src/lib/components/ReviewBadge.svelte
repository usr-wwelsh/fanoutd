<script>
  // Where one piece of work stands with a reviewer. It takes the state rather
  // than the task, so a plan header can hand it the rollup of five subtasks and
  // a card can hand it its own row, and both draw the same thing.
  let { state, round = 0, title = true } = $props();
</script>

{#if state}
  <span class="verdict {state.tone}" title={title ? state.hint : undefined}>
    <span class="mark {state.key}"></span>{state.label}
    {#if round > 0}<span class="rd">· R{round}</span>{/if}
  </span>
{/if}

<style>
  .rd { letter-spacing: .08em; opacity: .8; }
</style>
