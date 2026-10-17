<script>
  // The ZFS cache (ARC) limit, in Settings > Storage. ZFS fills spare RAM with
  // recently read data and gives it back when programs need memory, but
  // slowly; since OpenZFS 2.3 it may take all but 1 GB. A limit keeps room
  // for VMs and containers.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import { bytes, percent } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';

  const GB = 1024 ** 3;
  const HEADROOM = 4 * GB; // for the server itself and containers

  let arc = $state(null);
  let vmMem = $state(0); // memory of VMs that run now or start with the server
  let vmCount = $state(0);
  let want = $state(null); // the limit being edited, in GB
  let touched = false; // until you move it, it follows the suggestion
  let saving = $state(false);

  async function load() {
    try {
      arc = await api.get('/host/zfs/arc');
      // Start on the limit set, or on the suggestion when there's none.
      if (want === null && arc.available) want = arc.limit ? Math.round(arc.limit / GB) : null;
    } catch {
      arc = null;
    }
  }
  onMount(() => {
    load();
    api.get('/vms').then((vms) => {
      const counted = vms.filter((v) => v.state === 'running' || v.autostart);
      vmMem = counted.reduce((s, v) => s + v.memory, 0);
      vmCount = counted.length;
    }).catch(() => {});
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  });

  const minGB = $derived(arc ? Math.ceil(arc.minLimit / GB) : 1);
  const maxGB = $derived(arc ? Math.floor(arc.maxLimit / GB) : 1);
  const clampGB = (n) => Math.min(maxGB, Math.max(minGB, Math.floor(n)));
  // A quarter of RAM, less if the VMs and the server need it.
  const suggested = $derived(arc ? clampGB(Math.min(arc.memTotal / 4, arc.memTotal - vmMem - HEADROOM) / GB) : 0);
  const pct = (a) => (arc?.memTotal ? Math.min(100, (a / arc.memTotal) * 100) : 0);
  $effect(() => {
    if (arc?.available && !arc.limit && !touched && suggested) want = suggested;
  });
  const left = $derived(arc && want ? arc.memTotal - want * GB - vmMem : 0);
  const leftNow = $derived(arc ? arc.memTotal - arc.max - vmMem : 0); // with the limit in force
  const changed = $derived(arc && want !== null && want * GB !== (arc.limit || 0));

  async function apply(gb) {
    saving = true;
    try {
      arc = await api.put('/host/zfs/arc', { max: gb ? gb * GB : 0 });
      want = Math.round((arc.limit || arc.max) / GB);
      toast(gb ? `ZFS cache limited to ${gb} GB. It shrinks to that over the next few minutes, and the limit is kept after a reboot.` : "Back to ZFS's own limit (all but 1 GB of RAM).", 'success', 7000);
    } catch (e) {
      toast(e.message, 'error', 10000);
    }
    saving = false;
  }
</script>

{#if arc?.available}
  <section class="card">
    <div class="card-header">
      <h2>ZFS cache (ARC)</h2>
      <span class="dim small" title="Since the server started">{percent(arc.hitRate, 0)} of reads come from RAM</span>
    </div>
    <div class="card-body stack-v">
      <p class="intro">ZFS keeps recently read data in spare RAM so it's fast to read again. It hands memory back when programs need it, but not quickly, so a big cache can squeeze VMs and containers. A limit keeps room for them.</p>

      <div class="usage">
        <div class="bar" title="ZFS cache {bytes(arc.size)} · limit {bytes(arc.max)} · RAM {bytes(arc.memTotal, 0)}">
          <div class="used" style="width:{pct(arc.size)}%"></div>
          <div class="limit" style="left:{pct(arc.max)}%"></div>
        </div>
        <div class="legend">
          <span><span class="dot"></span>Using <strong>{bytes(arc.size)}</strong> of {bytes(arc.memTotal, 0)} RAM</span>
          <span><span class="tick"></span>Limit {bytes(arc.max, 0)}{arc.limit ? '' : " (ZFS's default)"}</span>
          {#if arc.size > arc.max * 1.02}<span class="dim">shrinking to the limit…</span>{/if}
        </div>
      </div>

      <div class="field">
        <span>Limit</span>
        <div class="row">
          <input type="range" min={minGB} max={maxGB} step="1" bind:value={want} oninput={() => (touched = true)} aria-label="Limit in GB" />
          <label class="num"><input type="number" min={minGB} max={maxGB} step="1" bind:value={want} oninput={() => (touched = true)} /> GB</label>
        </div>
        <div class="presets">
          <button class="sm" class:active={want === suggested} onclick={() => ((want = suggested), (touched = true))}>Suggested: {suggested} GB</button>
          <button class="sm" onclick={() => ((want = clampGB(arc.memTotal / 2 / GB)), (touched = true))}>Half the RAM: {clampGB(arc.memTotal / 2 / GB)} GB</button>
          <button class="sm ghost" onclick={() => apply(0)} disabled={saving || !arc.limit} title="All but 1 GB of RAM">Use ZFS's default</button>
        </div>
        <span class="hint">
          Suggested is a quarter of the RAM, or less where needed to leave room for {vmMem ? `your ${vmCount} VM${vmCount === 1 ? '' : 's'} (${bytes(vmMem, 0)}) and ` : ''}4 GB for the server and containers. A smaller cache only means more reads go to the disks.
        </span>
      </div>

      {#if leftNow < HEADROOM}
        <div class="note amber"><Icon name="alert" size={16} /><span><strong>Right now ZFS may grow to {bytes(arc.max, 0)}</strong>, leaving {bytes(Math.max(0, leftNow), 0)} for {vmMem ? 'VMs, ' : ''}the server and containers. Apply a limit to keep room for them.</span></div>
      {/if}
      {#if want && want * GB !== arc.max && left < HEADROOM}
        <div class="note amber"><Icon name="alert" size={16} /><span>With {bytes(want * GB, 0)} for the cache{vmMem ? ` and ${bytes(vmMem, 0)} for VMs` : ''}, only {bytes(Math.max(0, left), 0)} is left for the server and containers. ZFS gives memory back eventually, but a VM or container can be killed for lack of memory first.</span></div>
      {/if}
      {#if arc.saved !== (arc.limit || 0)}
        <div class="note"><Icon name="info" size={16} /><span>The limit for the next boot ({arc.saved ? bytes(arc.saved, 0) : "ZFS's default"}{arc.savedIn ? `, in ${arc.savedIn}` : ''}) isn't the one in force now. Apply a limit here to set both.</span></div>
      {/if}
      {#if arc.boot === 'updating'}
        <p class="hint"><Icon name="loader" size={13} class="spin" /> Updating the boot image, so the limit applies from the moment ZFS starts…</p>
      {:else if arc.boot?.startsWith('failed')}
        <div class="note red"><Icon name="alert" size={16} /><span>The limit is in force and saved, but the boot image couldn't be updated ({arc.boot.slice(8)}). Run <code>sudo update-initramfs -u -k all</code>.</span></div>
      {/if}

      <div class="row end">
        <span class="dim small">{arc.saved ? `Kept after a reboot (${arc.savedIn})` : 'No limit saved for the next boot.'}</span>
        <span class="spacer"></span>
        <button class="primary" disabled={saving || !changed || !want} onclick={() => apply(want)}>
          {#if saving}<Icon name="loader" size={14} class="spin" />{/if} Apply {want ? `${want} GB` : ''}
        </button>
      </div>
    </div>
  </section>
{/if}

<style>
  .intro { margin: 0; font-size: 13.5px; line-height: 1.55; color: var(--text-2); }
  .usage { display: grid; gap: 7px; }
  .bar { position: relative; height: 10px; border-radius: 99px; background: var(--surface-2); }
  .used { height: 100%; border-radius: 99px; background: var(--accent); transition: width 0.6s; }
  .limit { position: absolute; top: -3px; bottom: -3px; width: 2px; margin-left: -1px; background: var(--text); border-radius: 1px; }
  .legend { display: flex; gap: 16px; flex-wrap: wrap; font-size: 13px; color: var(--text-2); align-items: center; }
  .legend strong { color: var(--text); }
  .dot { display: inline-block; width: 9px; height: 9px; border-radius: 50%; background: var(--accent); margin-right: 6px; }
  .tick { display: inline-block; width: 2px; height: 11px; background: var(--text); margin-right: 6px; vertical-align: -1px; }
  .field { display: grid; gap: 8px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .row { display: flex; gap: 10px; align-items: center; }
  .row.end { flex-wrap: wrap; }
  input[type='range'] { flex: 1; height: auto; padding: 0; border: 0; accent-color: var(--accent); }
  .num { display: flex; align-items: center; gap: 6px; font-size: 13px; color: var(--text-2); }
  .num input { width: 84px; }
  .presets { display: flex; gap: 6px; flex-wrap: wrap; }
  .presets button.active { border-color: var(--accent); color: var(--accent-text); background: var(--accent-soft); }
  .hint { margin: 0; display: flex; gap: 6px; align-items: center; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; border-radius: 10px; font-size: 13px; line-height: 1.5; background: var(--surface-2); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note.red { background: var(--red-soft); color: var(--red-text); }
  .small { font-size: 12.5px; }
  .spacer { flex: 1; }
</style>
