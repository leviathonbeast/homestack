<script>
  // A dropdown menu. items: [{ label, icon, onclick, danger, disabled, divider, hint }]
  import Icon from '../lib/Icon.svelte';

  let { items = [], label = 'More actions', icon = 'more', text = '', size = '', align = 'right' } = $props();
  let open = $state(false);
  let root;
  let btn;
  let pos = $state({ top: 0, left: 0, right: 0, up: false });

  // The list is position: fixed so it isn't clipped by scrolling tables.
  function place() {
    const r = btn.getBoundingClientRect();
    const up = r.bottom + 260 > window.innerHeight && r.top > 260;
    pos = { top: up ? r.top - 4 : r.bottom + 4, left: r.left, right: window.innerWidth - r.right, up };
  }

  function outside(e) {
    if (open && root && !root.contains(e.target)) open = false;
  }
  function pick(item, e) {
    e.stopPropagation();
    if (item.disabled) return;
    open = false;
    item.onclick?.();
  }
</script>

<svelte:window onclick={outside} onkeydown={(e) => e.key === 'Escape' && (open = false)}
  onscroll={() => (open = false)} onresize={() => (open = false)} />

<div class="menu" bind:this={root}>
  <button
    bind:this={btn}
    class="{text ? '' : 'icon'} {size} {text ? '' : 'ghost'}"
    aria-label={label}
    aria-haspopup="menu"
    aria-expanded={open}
    onclick={(e) => { e.stopPropagation(); if (!open) place(); open = !open; }}
  >
    {#if icon}<Icon name={icon} />{/if}{text}{#if text}<Icon name="chevron-down" size={14} />{/if}
  </button>
  {#if open}
    <!-- svelte-ignore a11y_click_events_have_key_events -->
    <div class="items" role="menu" tabindex="-1" class:up={pos.up} onclick={(e) => e.stopPropagation()}
      style="top:{pos.top}px;{align === 'right' ? `right:${pos.right}px` : `left:${pos.left}px`}">
      {#each items.filter(Boolean) as item}
        {#if item.divider}
          <div class="divider"></div>
        {:else}
          <button role="menuitem" class="item" class:danger={item.danger} disabled={item.disabled} onclick={(e) => pick(item, e)}>
            {#if item.icon}<Icon name={item.icon} size={15} />{/if}
            <span>{item.label}{#if item.hint}<small>{item.hint}</small>{/if}</span>
          </button>
        {/if}
      {/each}
    </div>
  {/if}
</div>

<style>
  .menu { position: relative; display: inline-block; }
  .items {
    position: fixed; z-index: 150; min-width: 200px;
    background: var(--surface); border: 1px solid var(--border); border-radius: 10px;
    box-shadow: var(--shadow-lg); padding: 4px; animation: pop 0.1s ease-out; text-align: left;
    white-space: normal; max-width: min(320px, calc(100vw - 16px)); /* not a table cell's nowrap */
  }
  .items.up { transform: translateY(-100%); animation: none; }
  .item {
    display: flex; width: 100%; justify-content: flex-start; border: 0; background: transparent; height: auto;
    padding: 7px 10px; border-radius: 6px; font-weight: 400; color: var(--text); gap: 10px;
  }
  .item:hover { background: var(--surface-2); }
  .item span { display: grid; text-align: left; white-space: normal; }
  .item small { color: var(--text-3); font-size: 12px; }
  .item :global(svg) { color: var(--text-2); flex: none; }
  .item.danger, .item.danger :global(svg) { color: var(--red-text); }
  .item.danger:hover { background: var(--red-soft); }
  .divider { height: 1px; background: var(--border); margin: 4px 2px; }
  @keyframes pop { from { opacity: 0; transform: translateY(-3px); } }
</style>
