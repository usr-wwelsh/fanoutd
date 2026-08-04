<script>
  // A rail down the right edge that is the plan seen from far away: one shaded
  // block per thing on the page, in the place it actually occupies, measured
  // from the rendered DOM rather than recomputed from the plan. So a fanout too
  // tall to scan is one drag from end to end, and you can still tell what you
  // are landing on. It sits dim until the page moves and brightens while it is
  // moving, which is the only time it has anything to say.
  let { container, signature = '' } = $props();

  let blocks = $state([]);
  let docH = $state(0);
  let viewH = $state(0);
  let offset = $state(0);
  let lit = $state(false);
  let dragging = $state(false);

  let rail = $state(null);
  let fade;
  let frame = 0;

  let scale = $derived(docH > 0 ? viewH / docH : 0);
  let overflowing = $derived(docH - viewH > 160);
  let progress = $derived(docH > viewH ? Math.round((offset / (docH - viewH)) * 100) : 0);

  // Anything the map should draw marks itself with data-map, so the rail never
  // needs to know what a node or a row is made of — only where one ended up.
  function measure() {
    if (!container) return;
    docH = document.documentElement.scrollHeight;
    viewH = window.innerHeight;
    offset = window.scrollY;
    const box = container.getBoundingClientRect();
    const width = box.width || 1;
    blocks = [...container.querySelectorAll('[data-map]')].map(el => {
      const r = el.getBoundingClientRect();
      const x = Math.min(1, Math.max(0, (r.left - box.left) / width));
      return {
        kind: el.dataset.map,
        state: el.dataset.mapState ?? '',
        top: r.top + window.scrollY,
        h: r.height,
        x,
        w: Math.min(1 - x, r.width / width),
      };
    });
  }

  function schedule() {
    if (frame) return;
    frame = requestAnimationFrame(() => { frame = 0; measure(); });
  }

  function flash() {
    lit = true;
    clearTimeout(fade);
    fade = setTimeout(() => { if (!dragging) lit = false; }, 900);
  }

  $effect(() => {
    const onScroll = () => { offset = window.scrollY; flash(); };
    const onResize = () => schedule();
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onResize);
    const ro = new ResizeObserver(schedule);
    if (container) ro.observe(container);
    schedule();
    return () => {
      window.removeEventListener('scroll', onScroll);
      window.removeEventListener('resize', onResize);
      ro.disconnect();
      cancelAnimationFrame(frame);
      frame = 0;
      clearTimeout(fade);
    };
  });

  // Plans arrive after their tasks do, so the page keeps growing under the map;
  // re-measure whenever the board's own signature moves.
  $effect(() => { signature; schedule(); });

  function scrollTo(clientY) {
    const r = rail.getBoundingClientRect();
    const frac = (clientY - r.top) / r.height;
    const max = Math.max(0, docH - viewH);
    window.scrollTo({ top: Math.min(max, Math.max(0, frac * docH - viewH / 2)) });
  }

  function down(e) {
    dragging = true;
    lit = true;
    rail.setPointerCapture(e.pointerId);
    scrollTo(e.clientY);
    e.preventDefault();
  }

  function move(e) {
    if (dragging) scrollTo(e.clientY);
  }

  function up(e) {
    if (!dragging) return;
    dragging = false;
    if (rail.hasPointerCapture(e.pointerId)) rail.releasePointerCapture(e.pointerId);
    flash();
  }

  function key(e) {
    const step = { ArrowUp: -120, ArrowDown: 120, PageUp: -viewH * 0.9, PageDown: viewH * 0.9 }[e.key];
    if (step !== undefined) window.scrollBy({ top: step });
    else if (e.key === 'Home') window.scrollTo({ top: 0 });
    else if (e.key === 'End') window.scrollTo({ top: docH });
    else return;
    e.preventDefault();
  }
</script>

{#if overflowing}
  <div
    class="rail"
    class:lit
    class:dragging
    bind:this={rail}
    role="scrollbar"
    tabindex="0"
    aria-label="Plan overview"
    aria-controls="plan-view"
    aria-orientation="vertical"
    aria-valuemin="0"
    aria-valuemax="100"
    aria-valuenow={progress}
    onpointerdown={down}
    onpointermove={move}
    onpointerup={up}
    onpointercancel={up}
    onmouseenter={() => lit = true}
    onmouseleave={() => { if (!dragging) lit = false; }}
    onkeydown={key}
  >
    <div class="map">
      {#each blocks as b, i (i)}
        <div
          class="blk {b.kind} {b.state}"
          style="top:{b.top * scale}px; height:{Math.max(1.5, b.h * scale)}px;
                 left:{b.x * 100}%; width:{Math.max(6, b.w * 100)}%"
        ></div>
      {/each}
    </div>
    <div class="window" style="top:{offset * scale}px; height:{Math.max(14, viewH * scale)}px"></div>
  </div>
{/if}

<style>
  .rail {
    position: fixed;
    top: 0;
    right: 0;
    bottom: 0;
    width: 74px;
    background: var(--panel);
    border-left: 1px solid var(--rule);
    cursor: pointer;
    z-index: 12;
    touch-action: none;
  }
  .rail.dragging { cursor: grabbing; }

  .map { position: absolute; inset: 0 11px 0 13px; }

  /* Dim is the resting state: the map is only worth reading while you are
     moving through it, so scrolling, hovering and dragging all light it. */
  .blk {
    position: absolute;
    background: var(--rule-soft);
    opacity: .8;
    transition: background .35s ease, opacity .35s ease;
  }
  .blk.head { background: var(--rule); }
  .blk.running { background: var(--live); opacity: .55; }
  .blk.error { background: var(--fault); opacity: .55; }
  .blk.review { background: var(--judge); opacity: .55; }

  .rail.lit .blk { background: var(--ink-3); opacity: 1; transition-duration: .1s; }
  .rail.lit .blk.head { background: var(--ink-2); }
  .rail.lit .blk.running { background: var(--live); }
  .rail.lit .blk.error { background: var(--fault); }
  .rail.lit .blk.review { background: var(--judge); }

  .window {
    position: absolute;
    left: 0;
    right: 0;
    border-top: 1px solid var(--rule);
    border-bottom: 1px solid var(--rule);
    background: light-dark(rgba(31, 35, 32, .06), rgba(232, 230, 220, .06));
    transition: border-color .35s ease, background .35s ease;
    pointer-events: none;
  }
  .rail.lit .window {
    border-color: var(--ink);
    background: light-dark(rgba(31, 35, 32, .11), rgba(232, 230, 220, .1));
    transition-duration: .1s;
  }

  @media (prefers-reduced-motion: reduce) {
    .blk, .window { transition: none; }
  }

  @media (max-width: 720px) {
    .rail { display: none; }
  }
</style>
