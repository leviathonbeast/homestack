<script>
  // A right-click menu at the pointer. items: [{ label, icon, onclick, danger,
  // disabled, divider, hint, keys }]. Arrow keys move, Enter picks, Escape closes.
  import { tick } from 'svelte';
  import Icon from '../lib/Icon.svelte';

  let { x = 0, y = 0, items = [], onclose } = $props();
  let el = $state();
  let pos = $state({ left: 0, top: 0 });

  // Keep it on screen: flip left or up when it would run off the edge.
  $effect(() => {
    x;
    y;
    tick().then(() => {
      if (!el) return;
      const r = el.getBoundingClientRect();
      pos = {
        left: Math.max(4, x + r.width > innerWidth - 4 ? x - r.width : x),
        top: Math.max(4, y + r.height > innerHeight - 4 ? y - r.height : y),
      };
      el.querySelector('button:not([disabled])')?.focus();
    });
  });

  function pick(item) {
    if (item.disabled) return;
    onclose?.();
    item.onclick?.();
  }
  function onkeydown(e) {
    const buttons = [...el.querySelectorAll('button:not([disabled])')];
    const i = buttons.indexOf(document.activeElement);
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      buttons[(i + (e.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length]?.focus();
    } else if (e.key === 'Escape' || e.key === 'Tab') {
      e.preventDefault();
      onclose?.();
    }
  }
</script>

<svelte:window onscroll={() => onclose?.()} onresize={() => onclose?.()} />

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="backdrop" onpointerdown={() => onclose?.()} oncontextmenu={(e) => { e.preventDefault(); onclose?.(); }}></div>
<div class="items" role="menu" tabindex="-1" bind:this={el} style="left:{pos.left}px;top:{pos.top}px" {onkeydown}>
  {#each items.filter(Boolean) as item}
    {#if item.divider}
      <div class="divider"></div>
    {:else}
      <button role="menuitem" class="item" class:danger={item.danger} disabled={item.disabled} title={item.title || ''} onclick={() => pick(item)}>
        {#if item.icon}<Icon name={item.icon} size={15} />{/if}
        <span class="label">{item.label}{#if item.hint}<small>{item.hint}</small>{/if}</span>
        {#if item.keys}<kbd>{item.keys}</kbd>{/if}
      </button>
    {/if}
  {/each}
</div>

<style>
  .backdrop { position: fixed; inset: 0; z-index: 149; }
  .items {
    position: fixed; z-index: 150; min-width: 220px; max-width: 320px;
    background: var(--surface); border: 1px solid var(--border); border-radius: 10px;
    box-shadow: var(--shadow-lg); padding: 4px; animation: pop 0.08s ease-out;
  }
  .item {
    width: 100%; justify-content: flex-start; border: 0; background: transparent; height: auto;
    padding: 7px 10px; border-radius: 6px; font-weight: 400; color: var(--text); gap: 10px; text-align: left;
  }
  .item:hover:not(:disabled), .item:focus-visible { background: var(--surface-2); outline: none; }
  .item:disabled { opacity: 0.45; cursor: default; }
  .label { flex: 1; display: grid; min-width: 0; }
  .item small { color: var(--text-3); font-size: 12px; }
  .item :global(svg) { color: var(--text-2); flex: none; }
  .item.danger, .item.danger :global(svg) { color: var(--red-text); }
  .item.danger:hover:not(:disabled) { background: var(--red-soft); }
  kbd { font-family: var(--mono, ui-monospace, monospace); font-size: 11px; color: var(--text-3); }
  .divider { height: 1px; background: var(--border); margin: 4px 2px; }
  @keyframes pop { from { opacity: 0; transform: scale(0.98); } }
</style>
