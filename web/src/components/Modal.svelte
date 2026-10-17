<script>
  import Icon from '../lib/Icon.svelte';

  let { title, onclose, width = 460, children, footer, dismissable = true } = $props();

  function onkey(e) {
    if (e.key === 'Escape' && dismissable) onclose?.();
  }
</script>

<svelte:window onkeydown={onkey} />

<div class="backdrop" role="presentation" onclick={(e) => e.target === e.currentTarget && dismissable && onclose?.()}>
  <div class="modal" role="dialog" aria-modal="true" aria-label={title} style="max-width: {width}px">
    <header>
      <h2>{title}</h2>
      {#if dismissable}
        <button class="ghost icon sm" onclick={() => onclose?.()} aria-label="Close"><Icon name="x" /></button>
      {/if}
    </header>
    <div class="body">{@render children?.()}</div>
    {#if footer}
      <footer>{@render footer()}</footer>
    {/if}
  </div>
</div>

<style>
  .backdrop {
    position: fixed; inset: 0; z-index: 100;
    background: rgb(10 12 16 / 0.45);
    display: grid; place-items: center; padding: 16px;
    animation: fade 0.12s ease-out;
  }
  .modal {
    width: 100%; background: var(--surface); border: 1px solid var(--border);
    border-radius: 14px; box-shadow: var(--shadow-lg);
    animation: pop 0.14s ease-out; max-height: calc(100vh - 32px); display: flex; flex-direction: column;
  }
  header { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 16px 18px 0; }
  header h2 { font-size: 16px; }
  .body { padding: 12px 18px 18px; overflow: auto; color: var(--text-2); }
  footer { display: flex; justify-content: flex-end; gap: 8px; padding: 14px 18px; border-top: 1px solid var(--border); background: var(--surface-2); border-radius: 0 0 14px 14px; }
  @keyframes fade { from { opacity: 0; } }
  @keyframes pop { from { opacity: 0; transform: translateY(6px) scale(0.98); } }
</style>
