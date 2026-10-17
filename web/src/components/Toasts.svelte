<script>
  import { toasts, dismissToast } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';

  const icon = { success: 'check-circle', error: 'x-circle', info: 'info', warn: 'alert' };
</script>

<div class="toasts" aria-live="polite">
  {#each toasts.list as t (t.id)}
    <div class="toast {t.kind}">
      <Icon name={icon[t.kind] || 'info'} size={17} />
      <span>{t.message}</span>
      <button class="ghost icon sm" onclick={() => dismissToast(t.id)} aria-label="Dismiss"><Icon name="x" size={14} /></button>
    </div>
  {/each}
</div>

<style>
  .toasts { position: fixed; top: 16px; right: 16px; z-index: 200; display: grid; gap: 8px; width: min(380px, calc(100vw - 32px)); }
  .toast {
    display: flex; align-items: flex-start; gap: 10px; padding: 11px 8px 11px 14px;
    background: var(--surface); border: 1px solid var(--border); border-radius: 10px;
    box-shadow: var(--shadow-lg); font-size: 13.5px; animation: slide 0.18s ease-out;
  }
  .toast span { flex: 1; padding-top: 1px; overflow-wrap: anywhere; }
  .toast > :global(svg) { margin-top: 2px; flex: none; }
  .toast.success > :global(svg) { color: var(--green); }
  .toast.error > :global(svg) { color: var(--red); }
  .toast.warn > :global(svg) { color: var(--amber); }
  .toast.info > :global(svg) { color: var(--accent); }
  .toast button { margin: -4px 0; }
  @keyframes slide { from { opacity: 0; transform: translateY(-6px); } }
</style>
