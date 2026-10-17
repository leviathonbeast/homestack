<script module>
  // pageOf is the slice of items on a page. The page is clamped, so a list
  // that shrinks under a refresh never leaves an empty page showing.
  export function pageOf(items, page, size = 10) {
    const at = clamp(page, items.length, size);
    return items.slice(at * size, (at + 1) * size);
  }
  function clamp(page, total, size) {
    return Math.max(0, Math.min(page, Math.ceil(total / size) - 1));
  }
</script>

<script>
  import Icon from '../lib/Icon.svelte';

  // A footer for a newest-first list shown a page at a time (see pageOf).
  // Hidden while everything fits on one page.
  let { total, size = 10, page = $bindable(0) } = $props();
  const at = $derived(clamp(page, total, size));
  const last = $derived(Math.ceil(total / size) - 1);
</script>

{#if total > size}
  <div class="pager">
    <span class="dim small">{at * size + 1}–{Math.min(total, (at + 1) * size)} of {total}</span>
    <span class="spacer"></span>
    <button class="sm ghost" disabled={at === 0} onclick={() => (page = at - 1)}><Icon name="chevron-left" size={13} /> Newer</button>
    <button class="sm ghost" disabled={at >= last} onclick={() => (page = at + 1)}>Older <Icon name="chevron-right" size={13} /></button>
  </div>
{/if}

<style>
  .pager { display: flex; align-items: center; gap: 6px; padding: 8px 12px 8px 18px; border-top: 1px solid var(--border); }
  .spacer { flex: 1; }
  .small { font-size: 12.5px; }
</style>
