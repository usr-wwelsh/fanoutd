<script>
  // The fanout itself: waves top to bottom, one node per subtask, one edge per
  // derived dependency. Nothing here is authored — the shape is a consequence
  // of which paths each subtask claimed, so this is the plan's own picture of
  // itself rather than a diagram someone drew.
  //
  // Review continues the same picture below the last wave. A verdict is a gate
  // the assembled work passes through, and a rework is another wave under it, so
  // they are drawn as bands in the same column rather than listed somewhere
  // else: a rework belongs to the plan that produced it, and a plan whose work
  // was sent back is not finished with.
  import { createEventDispatcher } from 'svelte';

  let { plan, bands = [], selectedId = null } = $props();

  const dispatch = createEventDispatcher();

  const NW = 236;   // node width
  const NH = 98;    // node height
  const VW = 460;   // verdict plate width, capped to the graph below
  const VH = 76;    // verdict plate height
  const GAPX = 26;  // between siblings in a wave
  const GAPY = 116; // between waves, the band the edges cross
  const PADX = 92;  // left gutter, where the wave numbers live
  const PADY = 18;

  let byId = $derived(new Map((plan.tasks ?? []).map(t => [t.id, t])));

  // Ordinals number the subtasks in schedule order, so 01 is in the first wave
  // and the last number is whatever integrates the rest.
  let ordinals = $derived.by(() => {
    const out = new Map();
    let n = 0;
    for (const wave of plan.waves ?? []) {
      for (const id of wave) if (byId.has(id)) out.set(id, ++n);
    }
    return out;
  });

  let layout = $derived.by(() => {
    const rows = (plan.waves ?? []).map(ids => ids.filter(id => byId.has(id))).filter(r => r.length);
    const widths = rows.map(r => r.length * NW + (r.length - 1) * GAPX);
    const inner = Math.max(NW, ...widths, 0);
    const pos = new Map();

    let y = PADY;
    rows.forEach((row, wi) => {
      const x0 = PADX + (inner - widths[wi]) / 2;
      row.forEach((id, i) => pos.set(id, { x: x0 + i * (NW + GAPX), y, wave: wi + 1 }));
      y += NH + GAPY;
    });

    // The bands carry on down the same centre line the waves are laid out on,
    // so the review reads as the continuation of the schedule that it is.
    const placed = bands.map((band) => {
      const w = band.kind === 'verdict' ? Math.min(inner, VW) : NW;
      const h = band.kind === 'verdict' ? VH : NH;
      const at = { band, x: PADX + (inner - w) / 2, y, w, h };
      y += h + GAPY;
      return at;
    });

    return {
      rows,
      pos,
      bands: placed,
      width: PADX + inner + 24,
      height: rows.length || placed.length ? y - GAPY + PADY : 0,
    };
  });

  // Both halves of the drawing hang off the same three numbers, so an edge does
  // not need to know whether it is leaving a subtask or a verdict.
  function anchorOf(at, kind) {
    const w = kind === 'node' ? NW : at.w;
    const h = kind === 'node' ? NH : at.h;
    return { cx: at.x + w / 2, top: at.y, bottom: at.y + h };
  }

  function link(from, to, hot = false) {
    const mid = (from.bottom + to.top) / 2;
    return {
      d: `M${from.cx} ${from.bottom} C${from.cx} ${mid}, ${to.cx} ${mid}, ${to.cx} ${to.top}`,
      hot,
    };
  }

  let edges = $derived.by(() => {
    const out = [];
    for (const [id, deps] of Object.entries(plan.deps ?? {})) {
      const to = layout.pos.get(id);
      if (!to) continue;
      for (const dep of deps ?? []) {
        const from = layout.pos.get(dep);
        if (!from) continue;
        out.push({
          key: `${dep}>${id}`,
          ...link(anchorOf(from, 'node'), anchorOf(to, 'node'), byId.get(dep)?.status === 'running'),
        });
      }
    }
    return out;
  });

  // Everything in the last wave converges on the first verdict, because that is
  // what was judged: the assembled work, not the subtask the trace happens to
  // hang on. After that the chain is a straight line down.
  let bandEdges = $derived.by(() => {
    const out = [];
    layout.bands.forEach((at, i) => {
      const to = anchorOf(at, 'band');
      if (i === 0) {
        const last = layout.rows[layout.rows.length - 1] ?? [];
        for (const id of last) {
          const from = layout.pos.get(id);
          if (from) out.push({ key: `${id}>b0`, review: true, ...link(anchorOf(from, 'node'), to) });
        }
        return;
      }
      const from = anchorOf(layout.bands[i - 1], 'band');
      out.push({ key: `b${i - 1}>b${i}`, review: true, ...link(from, to) });
    });
    return out;
  });

  // A running subtask is the only thing in colour, so it also carries the only
  // label: what it owns, and therefore what is waiting on it.
  let liveLabels = $derived.by(() => {
    const out = [];
    for (const [id, task] of byId) {
      if (task.status !== 'running') continue;
      const at = layout.pos.get(id);
      if (!at) continue;
      const blocked = Object.entries(plan.deps ?? {})
        .filter(([, deps]) => (deps ?? []).includes(id))
        .map(([reader]) => ordinals.get(reader))
        .filter(Boolean)
        .sort((a, b) => a - b);
      const paths = (plan.writes?.[id] ?? []);
      if (!paths.length) continue;
      out.push({
        id,
        x: at.x + NW / 2,
        y: at.y + NH + 26,
        text: blocked.length
          ? `${paths[0]} blocks ${blocked.map(n => String(n).padStart(2, '0')).join(', ')}`
          : paths[0],
      });
    }
    return out;
  });

  function pad(n) { return String(n).padStart(2, '0'); }
  function paths(list) { return list?.length ? list.join(', ') : '—'; }

  // The gutter numbers the review bands the way it numbers the waves, in the
  // same two lines: what this row is, and what it is for.
  function bandLabel(band) {
    if (band.kind === 'rework') return { n: `R${band.task.review_round}`, sub: 'rework' };
    return { n: 'RV', sub: 'verdict' };
  }
</script>

{#if layout.rows.length}
  <div class="scroll">
    <div class="canvas" style="width:{layout.width}px; height:{layout.height}px">
      <svg viewBox="0 0 {layout.width} {layout.height}" width={layout.width} height={layout.height} aria-hidden="true">
        <defs>
          <marker id="pg-head" markerWidth="7" markerHeight="7" refX="6.2" refY="3.5" orient="auto">
            <path d="M0 0 L7 3.5 L0 7 z" fill="var(--rule)" />
          </marker>
          <marker id="pg-head-hot" markerWidth="7" markerHeight="7" refX="6.2" refY="3.5" orient="auto">
            <path d="M0 0 L7 3.5 L0 7 z" fill="var(--live)" />
          </marker>
          <marker id="pg-head-judge" markerWidth="7" markerHeight="7" refX="6.2" refY="3.5" orient="auto">
            <path d="M0 0 L7 3.5 L0 7 z" fill="var(--judge)" />
          </marker>
        </defs>

        {#each layout.rows as _, wi}
          <line
            class="waverule"
            x1="16" x2={layout.width - 16}
            y1={PADY + wi * (NH + GAPY) - 14}
            y2={PADY + wi * (NH + GAPY) - 14}
          />
        {/each}

        {#each layout.bands as at}
          <line class="waverule judge" x1="16" x2={layout.width - 16} y1={at.y - 14} y2={at.y - 14} />
        {/each}

        {#each edges as edge (edge.key)}
          <path
            class="edge"
            class:hot={edge.hot}
            d={edge.d}
            marker-end="url(#{edge.hot ? 'pg-head-hot' : 'pg-head'})"
          />
        {/each}

        {#each bandEdges as edge (edge.key)}
          <path class="edge judge" d={edge.d} marker-end="url(#pg-head-judge)" />
        {/each}
      </svg>

      {#each layout.rows as row, wi}
        <div class="wave" style="top:{PADY + wi * (NH + GAPY)}px">
          <div class="wave-n">W{wi + 1}</div>
          <div class="eyebrow wave-sub">
            {wi === layout.rows.length - 1 && row.length === 1 && wi > 0
              ? 'integration'
              : `${row.length} parallel`}
          </div>
        </div>
      {/each}

      {#each layout.bands as at (at.band.key)}
        {@const label = bandLabel(at.band)}
        <div class="wave judge" style="top:{at.y}px">
          <div class="wave-n">{label.n}</div>
          <div class="eyebrow wave-sub">{label.sub}</div>
        </div>
      {/each}

      {#each [...layout.pos] as [id, at] (id)}
        {@const task = byId.get(id)}
        <button
          class="node {task.status}"
          class:selected={selectedId === id}
          data-map="node"
          data-map-state={task.status}
          style="left:{at.x}px; top:{at.y}px"
          onclick={() => dispatch('selectTask', { taskId: id })}
        >
          <span class="node-head">
            <span class="mark {task.status}"></span>
            <span class="ord">{pad(ordinals.get(id) ?? 0)}</span>
            <span class="st">{task.status}</span>
          </span>
          <span class="node-title">{task.title}</span>
          <span class="io">
            <span class="k">writes</span><span class="v">{paths(plan.writes?.[id])}</span>
            <span class="k">reads</span><span class="v">{paths(plan.reads?.[id])}</span>
          </span>
        </button>
      {/each}

      {#each layout.bands as at (at.band.key)}
        {@const band = at.band}
        {#if band.kind === 'verdict'}
          <button
            class="plate {band.state.tone}"
            style="left:{at.x}px; top:{at.y}px; width:{at.w}px; height:{at.h}px"
            data-map="node"
            data-map-state="review"
            title="Open the review agent's own trace"
            onclick={() => dispatch('selectTask', { taskId: band.traceId, focus: 'review' })}
          >
            <span class="plate-head">
              <span class="mark {band.state.key}"></span>
              <span class="pl">{band.state.label}</span>
              {#if band.round > 0}<span class="rd">round {band.round}</span>{/if}
              <span class="open">review trace →</span>
            </span>
            <span class="plate-note">{band.note}</span>
          </button>
        {:else}
          <button
            class="node rework {band.task.status}"
            class:selected={selectedId === band.task.id}
            style="left:{at.x}px; top:{at.y}px"
            data-map="node"
            data-map-state={band.task.status}
            onclick={() => dispatch('selectTask', { taskId: band.task.id })}
          >
            <span class="node-head">
              <span class="mark {band.task.status}"></span>
              <span class="ord">rework {band.task.review_round}</span>
              <span class="st">{band.task.status}</span>
            </span>
            <span class="node-title">{band.task.title}</span>
            <span class="io one">
              <span class="k">fixing</span><span class="v">{band.task.goal || '—'}</span>
            </span>
          </button>
        {/if}
      {/each}

      {#each liveLabels as label (label.id)}
        <div class="live-label" style="left:{label.x}px; top:{label.y}px">{label.text}</div>
      {/each}
    </div>
  </div>
{/if}

<style>
  .scroll { overflow-x: auto; padding-bottom: 6px; }
  .canvas { position: relative; }
  .canvas svg { position: absolute; inset: 0; }

  .waverule { stroke: var(--rule); stroke-width: 1; stroke-dasharray: 2 6; }
  .waverule.judge { stroke: var(--judge); opacity: .5; }
  .edge { fill: none; stroke: var(--rule); stroke-width: 1.25; }
  .edge.hot { stroke: var(--live); stroke-width: 1.75; }
  .edge.judge { stroke: var(--judge); stroke-dasharray: 4 4; }

  .wave { position: absolute; left: 16px; width: 76px; }
  .wave-n {
    font-family: var(--f-mono);
    font-size: 21px;
    line-height: 1;
    letter-spacing: .03em;
  }
  .wave.judge .wave-n { color: var(--judge); }
  .wave-sub { display: block; margin-top: 6px; }

  .node {
    position: absolute;
    width: 236px;
    height: 98px;
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 9px 12px 10px;
    text-align: left;
    font: inherit;
    color: inherit;
    background: var(--panel);
    border: 1px solid var(--rule);
    border-radius: 0;
    cursor: pointer;
    transition: border-color .12s, box-shadow .12s;
  }
  .node:hover { border-color: var(--ink); }
  .node.selected { border-color: var(--ink); box-shadow: inset 0 0 0 1px var(--ink); }
  .node.running { border-color: var(--live); box-shadow: inset 3px 0 0 var(--live); }
  .node.running.selected { box-shadow: inset 3px 0 0 var(--live), inset 0 0 0 1px var(--live); }
  .node.error { border-color: var(--fault); box-shadow: inset 3px 0 0 var(--fault); }
  .node.idle { background: var(--sunk); }

  .node.rework { border-color: var(--judge); }
  .node.rework .node-head { color: var(--judge); }
  .node.rework.running { border-color: var(--live); }
  .node.rework.running .node-head { color: var(--live); }

  .node-head {
    display: flex;
    align-items: center;
    gap: 7px;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .15em;
    text-transform: uppercase;
    color: var(--ink-3);
  }
  .node-head .st { margin-left: auto; }
  .node.running .node-head { color: var(--live); }
  .node.error .node-head { color: var(--fault); }

  .node-title {
    flex: 1;
    min-height: 0;
    font-size: 13.5px;
    font-weight: 500;
    line-height: 1.3;
    overflow: hidden;
    display: -webkit-box;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;
  }
  .node.idle .node-title { color: var(--ink-2); }

  .io {
    display: grid;
    grid-template-columns: 40px 1fr;
    gap: 0 7px;
    padding-top: 6px;
    border-top: 1px solid var(--rule-soft);
    font-family: var(--f-mono);
    font-size: 10px;
    line-height: 1.55;
  }
  .io .k { color: var(--ink-3); letter-spacing: .08em; }
  .io .v { color: var(--ink-2); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .io.one { grid-template-columns: 44px 1fr; }

  /* The verdict is a plate the work passes through rather than a box of its own:
     wider than a subtask, shorter, and ruled in the colour of a held answer. */
  .plate {
    position: absolute;
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 9px 13px 10px;
    text-align: left;
    font: inherit;
    color: inherit;
    background: var(--judge-wash);
    border: 1px solid var(--judge);
    border-radius: 0;
    cursor: pointer;
    transition: box-shadow .12s;
  }
  .plate:hover { box-shadow: inset 0 0 0 1px var(--judge); }
  .plate.fault { background: var(--fault-wash); border-color: var(--fault); }
  .plate.fault:hover { box-shadow: inset 0 0 0 1px var(--fault); }
  .plate.done { background: var(--panel); border-color: var(--rule); }
  .plate.done:hover { box-shadow: inset 0 0 0 1px var(--ink); }

  .plate-head {
    display: flex;
    align-items: center;
    gap: 8px;
    font-family: var(--f-mono);
    font-size: 10px;
    letter-spacing: .15em;
    text-transform: uppercase;
    color: var(--judge);
  }
  .plate.fault .plate-head { color: var(--fault); }
  .plate.done .plate-head { color: var(--ink-2); }
  .plate-head .rd { color: var(--ink-3); letter-spacing: .08em; }
  .plate-head .open { margin-left: auto; color: var(--ink-3); letter-spacing: .08em; }
  .plate:hover .open { color: var(--ink); }

  .plate-note {
    font-size: 12px;
    line-height: 1.4;
    color: var(--ink-2);
    overflow: hidden;
    display: -webkit-box;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;
  }

  .live-label {
    position: absolute;
    transform: translateX(-50%);
    padding: 2px 7px;
    background: var(--live);
    color: var(--on-live);
    font-family: var(--f-mono);
    font-size: 10px;
    white-space: nowrap;
    pointer-events: none;
  }
</style>
