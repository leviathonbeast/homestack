<script>
  // Settings → Storage: spin idle hard disks down, like Unraid's spin down
  // delay. The agent watches each disk's reads and writes and puts one in
  // standby after this long without any (internal/agent/spindown.go).
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';

  let v = $state(null);
  let agentError = $state('');
  let saving = $state(false);

  async function load() {
    try {
      v = await api.get('/host/spindown');
      agentError = '';
    } catch (e) {
      agentError = e.message;
    }
  }
  onMount(load);

  const label = (m) => (m === 0 ? 'Never' : m < 60 ? `${m} minutes` : m === 60 ? '1 hour' : `${m / 60} hours`);

  async function save(delayMinutes) {
    saving = true;
    try {
      v = await api.put('/host/spindown', { delayMinutes });
      toast(delayMinutes ? `Idle hard disks spin down after ${label(delayMinutes)}.` : 'Disks no longer spin down by themselves.', 'success', 4000);
    } catch (e) {
      toast(e.message, 'error', 8000);
      load();
    }
    saving = false;
  }
</script>

<section class="card">
  <div class="card-header"><h2>Spin down</h2></div>
  {#if agentError}
    <div class="card-body dim small">{agentError}</div>
  {:else if !v}
    <div class="card-body"><div class="skeleton" style="height:90px"></div></div>
  {:else}
    <div class="card-body stack-v">
      <p class="intro">A hard disk that hasn't been read or written for this long is put in standby: quieter, cooler and a few watts less each. It spins up again by itself (in a few seconds) when something reads it. Nothing spins down while a parity job, the mover or a disk job runs.</p>
      <label class="field">
        <span>Spin down idle hard disks after</span>
        <select value={v.delayMinutes} onchange={(e) => save(+e.target.value)} disabled={saving} style="max-width:220px">
          {#each v.delays as m}<option value={m}>{label(m)}</option>{/each}
        </select>
        <span class="hint">Applies to the {v.disks} SATA hard disk{v.disks === 1 ? '' : 's'} (not SSDs, NVMe or the boot drive). Each one can also be spun down now from its menu on Storage. Very short delays mean more spin-ups, which wear a disk more than running does: 30 minutes or more is usual.</span>
      </label>
      {#if v.problem}<div class="note amber"><Icon name="alert" size={15} /><span>{v.problem}</span></div>{/if}
    </div>
  {/if}
</section>

<style>
  .intro { margin: 0; font-size: 13.5px; line-height: 1.55; color: var(--text-2); }
  .field { display: grid; gap: 6px; font-size: 13px; }
  .field > span:first-child { font-weight: 500; }
  .hint { color: var(--text-2); font-size: 12.5px; line-height: 1.5; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; border-radius: 10px; font-size: 13px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note :global(svg) { flex: none; margin-top: 2px; }
</style>
