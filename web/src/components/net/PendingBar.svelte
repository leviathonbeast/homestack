<script>
  import { api } from '../../lib/api.js';
  import { toast } from '../../lib/state.svelte.js';
  import Icon from '../../lib/Icon.svelte';

  // A change to the server's network that's been applied but not kept yet.
  // Keeping it is the proof that this page can still reach Homestack; if
  // nobody keeps it, the agent puts the old settings back on its own.
  let { pending, unreachable = false, onchange } = $props();

  let left = $state(0);
  let busy = $state(false);

  $effect(() => {
    left = pending.seconds;
    const t = setInterval(() => {
      left = Math.max(0, left - 1);
      if (left === 0) onchange?.();
    }, 1000);
    return () => clearInterval(t);
  });

  async function keep() {
    busy = true;
    try {
      await api.post('/network/host/confirm', { token: pending.token });
      toast('Kept the network change', 'success');
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    busy = false;
    onchange?.();
  }

  async function undo() {
    busy = true;
    try {
      await api.post('/network/host/cancel', { token: pending.token });
      toast('Undone: the previous network settings are back', 'success', 5000);
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    busy = false;
    onchange?.();
  }
</script>

<section class="bar" role="alert">
  <div class="head">
    <Icon name="shield" size={20} />
    <div class="grow">
      <strong>Keep this network change?</strong>
      <span>{pending.summary}</span>
    </div>
    <div class="count" class:low={left <= 20}>{left}s</div>
    <button onclick={undo} disabled={busy}>Undo now</button>
    <button class="primary" onclick={keep} disabled={busy || unreachable}><Icon name="check" size={15} /> Keep it</button>
  </div>
  <p class="why">
    {#if unreachable}
      This page can't reach Homestack at the moment. If that lasts, the change is undone automatically when the countdown ends.
    {:else}
      It's working well enough for this page to reach Homestack. Check what matters to you (apps, shares, VMs), then keep it. Stay on this page: if nobody keeps it, the old settings come back by themselves.
    {/if}
  </p>
  {#if pending.checks?.length}
    <div class="checks">
      {#each pending.checks as c}
        <span class="chk" class:ok={c.after} class:bad={!c.after && c.before} title="Tested from {c.from}">
          <Icon name={c.after ? 'check' : 'x'} size={13} /> {c.link} → gateway {c.gateway}{#if !c.after && !c.before} (didn't answer before either){/if}
        </span>
      {/each}
    </div>
  {/if}
  {#if pending.warnings?.length}
    <details>
      <summary>systemd-networkd reported {pending.warnings.length} warning{pending.warnings.length > 1 ? 's' : ''}</summary>
      <ul>{#each pending.warnings as w}<li class="mono">{w}</li>{/each}</ul>
    </details>
  {/if}
</section>

<style>
  .bar { position: sticky; top: 8px; z-index: 5; background: var(--surface); border: 1px solid var(--amber); border-radius: 12px; padding: 14px 16px; margin-bottom: 18px; box-shadow: var(--shadow); }
  .head { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
  .head > :global(svg) { color: var(--amber-text); flex: none; }
  .grow { flex: 1; min-width: 200px; display: grid; gap: 2px; }
  .grow span { font-size: 13px; color: var(--text-2); }
  .count { font-size: 20px; font-weight: 600; font-variant-numeric: tabular-nums; min-width: 52px; text-align: right; }
  .count.low { color: var(--red-text); }
  .why { font-size: 13px; color: var(--text-2); margin: 8px 0 0; line-height: 1.5; }
  .checks { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 10px; }
  .chk { display: inline-flex; align-items: center; gap: 5px; font-size: 12px; padding: 3px 9px; border-radius: 999px; background: var(--surface-2); color: var(--text-2); }
  .chk.ok { background: var(--green-soft); color: var(--green-text); }
  .chk.bad { background: var(--red-soft); color: var(--red-text); }
  details { margin-top: 8px; font-size: 12.5px; }
  summary { cursor: pointer; color: var(--text-2); }
  ul { margin: 6px 0 0 18px; padding: 0; }
  .mono { font-family: var(--mono, ui-monospace, monospace); font-size: 12px; }
</style>
