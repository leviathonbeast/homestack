<script>
  // Settings → Storage: how the parity jobs behave. How much of the array each
  // check reads and how old a block must be before it's checked again, and
  // the sync safety limits. The agent writes them for snapraid-job
  // (internal/agent/parity.go); without them the alerts settings decide.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';

  let v = $state(null);
  let f = $state({});
  let saving = $state(false);
  let agentError = $state('');

  const fields = [
    { key: 'SNAPRAID_SCRUB_PERCENT', label: 'Each check reads', unit: '% of the array', min: 1, max: 100 },
    { key: 'SNAPRAID_SCRUB_AGE', label: 'From blocks not checked for', unit: 'days', min: 0, max: 3650 },
    { key: 'SNAPRAID_DELETE_LIMIT', label: 'A sync waits when more files than this were deleted', unit: 'files', min: 0, max: 100000000 },
    { key: 'SNAPRAID_UPDATE_LIMIT', label: 'Or modified', unit: 'files', min: 0, max: 100000000 },
  ];

  async function load() {
    try {
      v = await api.get('/host/parity');
      f = { ...v.values };
      agentError = '';
    } catch (e) {
      agentError = e.message;
    }
  }
  onMount(load);

  const changed = $derived(v && fields.some((x) => +f[x.key] !== v.values[x.key]));
  // A full pass over the array takes this many weekly checks.
  const passWeeks = $derived(f.SNAPRAID_SCRUB_PERCENT > 0 ? Math.ceil(100 / f.SNAPRAID_SCRUB_PERCENT) : 0);

  async function save(values) {
    saving = true;
    try {
      v = await api.put('/host/parity', { values });
      f = { ...v.values };
      toast(Object.keys(values).length ? 'Saved. The next parity check and sync use these.' : 'Back to the alerts settings.', 'success', 4000);
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    saving = false;
  }
</script>

<section class="card">
  <div class="card-header"><h2>Parity</h2></div>
  {#if agentError}
    <div class="card-body dim small">{agentError}</div>
  {:else if !v}
    <div class="card-body"><div class="skeleton" style="height:120px"></div></div>
  {:else}
    <form class="card-body stack-v" onsubmit={(e) => { e.preventDefault(); save(Object.fromEntries(fields.map((x) => [x.key, Math.round(+f[x.key])]))); }}>
      <p class="intro">A parity check (a SnapRAID scrub) reads part of the array each time it runs and checks it against parity, oldest-checked first. A sync waits for you when too much changed since the last one, so a mistake or ransomware can't be written into parity.</p>
      <div class="grid">
        {#each fields as x (x.key)}
          <label class="field">
            <span>{x.label}</span>
            <span class="row"><input type="number" min={x.min} max={x.max} step="1" bind:value={f[x.key]} /> <span class="dim small">{x.unit}</span></span>
            {#if v.defaults[x.key] !== f[x.key]}<span class="hint">Without this: {v.defaults[x.key]}</span>{/if}
          </label>
        {/each}
      </div>
      {#if passWeeks}<p class="hint"><Icon name="info" size={13} /> Run weekly, that checks the whole array about every {passWeeks} weeks{passWeeks > 26 ? ': a long time for a fault to go unnoticed' : ''}. When it runs is on <a href="/schedules">Schedules</a>.</p>{/if}
      <div class="row">
        <button class="primary" type="submit" disabled={saving || !changed}>{#if saving}<Icon name="loader" size={14} class="spin" />{/if} Save</button>
        {#if v.custom}<button type="button" class="ghost" onclick={() => save({})} disabled={saving}>Use the alerts settings instead</button>{/if}
      </div>
    </form>
  {/if}
</section>

<style>
  .intro { margin: 0; font-size: 13.5px; line-height: 1.55; color: var(--text-2); }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 230px), 1fr)); gap: 14px 20px; }
  .field { display: grid; gap: 6px; font-size: 13px; align-content: start; }
  .field > span:first-child { font-weight: 500; }
  .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .row input { width: 130px; }
  .hint { margin: 0; color: var(--text-2); font-size: 12.5px; display: flex; gap: 6px; align-items: center; }
  .small { font-size: 12.5px; }
  p { margin: 0; }
</style>
