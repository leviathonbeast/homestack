<script>
  import { subscribe } from '../lib/api.js';
  import { nameColor } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';

  // path: SSE endpoint, e.g. /stacks/web/logs or /containers/<id>/logs
  let { path, height = 'calc(100vh - 290px)' } = $props();

  const MAX = 3000;
  let lines = $state.raw([]);
  let containers = $state([]);
  let hidden = $state({}); // container id -> true when filtered out
  let search = $state('');
  let showTime = $state(true);
  let wrap = $state(true);
  let follow = $state(true);
  let connected = $state(false);
  let box;
  let seq = 0;

  const ansi = /\x1b\[[0-9;?]*[A-Za-z]/g;

  $effect(() => {
    const p = path;
    lines = [];
    containers = [];
    const close = subscribe(p + '?tail=300', {
      open: () => (connected = true),
      error: () => (connected = false),
      containers: (list) => (containers = list),
      logs: (batch) => {
        connected = true;
        const next = lines.slice();
        for (const l of batch) {
          const line = { id: ++seq, c: l.c, n: l.n, s: l.s, t: l.t || '', l: (l.l || '').replace(ansi, '') };
          // Lines from different containers can arrive out of order; keep them sorted by time.
          let i = next.length;
          while (i > 0 && next[i - 1].t > line.t && next.length - i < 400) i--;
          next.splice(i, 0, line);
        }
        if (next.length > MAX) next.splice(0, next.length - MAX);
        lines = next;
      },
    });
    return close;
  });

  const needle = $derived(search.trim().toLowerCase());
  const visible = $derived(
    lines.filter((l) => !hidden[l.c] && (!needle || l.l.toLowerCase().includes(needle) || l.n.toLowerCase().includes(needle))),
  );

  $effect(() => {
    visible.length;
    if (follow && box) requestAnimationFrame(() => (box.scrollTop = box.scrollHeight));
  });

  function onscroll() {
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
    if (atBottom !== follow) follow = atBottom;
  }
  function jump() {
    follow = true;
    box.scrollTop = box.scrollHeight;
  }
  function time(t) {
    if (!t) return '';
    const d = new Date(t);
    return isNaN(d) ? '' : d.toLocaleTimeString(undefined, { hour12: false });
  }
  function download() {
    const text = visible.map((l) => `${l.t} ${l.n} ${l.l}`).join('\n');
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
    a.download = `homestack-logs-${new Date().toISOString().slice(0, 19)}.log`;
    a.click();
    URL.revokeObjectURL(a.href);
  }
  function highlight(text) {
    if (!needle) return [{ t: text }];
    const out = [];
    const lower = text.toLowerCase();
    let i = 0;
    for (;;) {
      const j = lower.indexOf(needle, i);
      if (j < 0) break;
      if (j > i) out.push({ t: text.slice(i, j) });
      out.push({ t: text.slice(j, j + needle.length), m: true });
      i = j + needle.length;
    }
    out.push({ t: text.slice(i) });
    return out;
  }
</script>

<div class="viewer">
  <div class="toolbar">
    <div class="search grow">
      <Icon name="search" size={15} />
      <input placeholder="Filter logs" bind:value={search} spellcheck="false" />
    </div>
    {#if containers.length > 1}
      <div class="chips">
        {#each containers as c (c.id)}
          <button class="chip" class:off={hidden[c.id]} onclick={() => (hidden[c.id] = !hidden[c.id])} title={hidden[c.id] ? 'Show' : 'Hide'}>
            <span class="sw" style="background:{nameColor(c.name)}"></span>{c.name}
          </button>
        {/each}
      </div>
    {/if}
    <div class="row">
      <button class="sm" class:on={showTime} onclick={() => (showTime = !showTime)} title="Show timestamps"><Icon name="clock" size={14} /><span class="hide-sm">Time</span></button>
      <button class="sm" class:on={wrap} onclick={() => (wrap = !wrap)} title="Wrap long lines"><Icon name="wrap" size={14} /><span class="hide-sm">Wrap</span></button>
      <button class="sm icon" onclick={() => (lines = [])} title="Clear" aria-label="Clear"><Icon name="trash" size={14} /></button>
      <button class="sm icon" onclick={download} title="Download" aria-label="Download"><Icon name="download" size={14} /></button>
    </div>
  </div>

  <div class="out" class:wrap bind:this={box} {onscroll} style="height:{height}">
    {#if !lines.length}
      <div class="placeholder">{connected ? 'No log output yet.' : 'Connecting…'}</div>
    {/if}
    {#each visible as l (l.id)}
      <div class="line" class:err={l.s === 2}>
        {#if showTime}<span class="ts">{time(l.t)}</span>{/if}
        {#if containers.length > 1}<span class="name" style="color:{nameColor(l.n)}">{l.n}</span>{/if}
        <span class="text">{#each highlight(l.l) as part}{#if part.m}<mark>{part.t}</mark>{:else}{part.t}{/if}{/each}</span>
      </div>
    {/each}
  </div>
  <div class="status">
    <span class="dot" class:green={connected}></span>
    <span>{connected ? 'Live' : 'Reconnecting'}</span>
    <span class="dim">· {visible.length.toLocaleString()} lines{needle ? ' match' : ''}</span>
    <span class="spacer"></span>
    {#if !follow}
      <button class="sm primary" onclick={jump}><Icon name="arrowdown" size={14} /> Jump to latest</button>
    {/if}
  </div>
</div>

<style>
  .viewer { display: grid; gap: 10px; }
  .toolbar { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .grow { flex: 1 1 220px; }
  .toolbar input { height: 32px; }
  .chips { display: flex; gap: 6px; flex-wrap: wrap; }
  .chip { height: 28px; padding: 0 10px; border-radius: 999px; font-size: 12.5px; gap: 6px; }
  .chip.off { opacity: 0.45; text-decoration: line-through; }
  .sw { width: 8px; height: 8px; border-radius: 50%; }
  button.on { background: var(--accent-soft); border-color: color-mix(in srgb, var(--accent) 35%, var(--border)); color: var(--accent-text); }
  .out {
    background: var(--log-bg); color: var(--log-text); border-radius: 10px; border: 1px solid var(--border);
    overflow: auto; padding: 10px 0; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; min-height: 260px;
  }
  .line { display: flex; gap: 12px; padding: 0 14px; white-space: pre; }
  .out.wrap .line .text { white-space: pre-wrap; word-break: break-word; }
  .line:hover { background: rgb(255 255 255 / 0.04); }
  .line.err .text { color: #fca5a5; }
  .ts { color: var(--log-dim); flex: none; user-select: none; }
  .name { flex: none; min-width: 90px; max-width: 160px; overflow: hidden; text-overflow: ellipsis; }
  .text { flex: 1; min-width: 0; }
  mark { background: #facc15; color: #111; border-radius: 2px; }
  .placeholder { color: var(--log-dim); padding: 6px 14px; font-family: var(--font); }
  .status { display: flex; align-items: center; gap: 8px; font-size: 12.5px; color: var(--text-2); min-height: 28px; }
  .spacer { flex: 1; }
</style>
