<script>
  // Settings → Storage: where the shares, the cache and the disks are
  // mounted. It tells Homestack (and the mover and alert jobs) where to
  // look; it doesn't mount or move anything. The agent checks a change
  // against what's mounted before saving it (internal/agent/layout.go).
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';

  let v = $state(null);
  let f = $state(null); // the form
  let saving = $state(false);
  let agentError = $state('');

  async function load() {
    try {
      v = await api.get('/host/storage/layout');
      f = { ...v.layout };
      agentError = '';
    } catch (e) {
      agentError = e.message;
    }
  }
  onMount(load);

  const fields = [
    { key: 'shares', label: 'Shares', hint: 'The cache and the data disks as one (mergerfs). Shares and apps use it.', view: true },
    { key: 'array', label: 'Array', hint: 'The data disks only (mergerfs). The mover moves files here.', view: true },
    { key: 'cache', label: 'Cache', hint: 'The fast disk new files land on.', view: true },
    { key: 'appdata', label: "Apps' data", hint: "A folder in the shares that's bound straight to the cache. Leave it empty if it isn't a mount of its own.", view: true },
    { key: 'diskPrefix', label: 'Data disks', hint: 'Followed by the disk number.', prefix: 'disk' },
    { key: 'parityPrefix', label: 'Parity disks', hint: 'Followed by the parity level.', prefix: 'parity' },
    { key: 'unassigned', label: 'Unassigned disks', hint: 'Other disks, each in a folder of its own.' },
    { key: 'snapraid', label: "SnapRAID's config", hint: '' },
    { key: 'owner', label: 'Owner', hint: '' },
  ];

  const changed = $derived(v && f && fields.some((x) => (f[x.key] || '').trim() !== (v.layout[x.key] || '')));
  const isDefault = $derived(v && f && fields.every((x) => (f[x.key] || '').trim() === (v.default[x.key] || '')));
  const base = (p) => (p || '').split('/').pop();
  const num = (prefix, p) => (p.startsWith(prefix) && /^[1-9]\d{0,2}$/.test(p.slice(prefix.length)) ? +p.slice(prefix.length) : 0);
  // The disks in snapraid.conf that aren't where the prefix says.
  const off = (key) => {
    const p = (f?.[key] || '').trim();
    const list = key === 'diskPrefix' ? v.disks : v.parity;
    return p ? list.filter((d) => !num(p, d)) : [];
  };
  const mounted = (key) => {
    const p = (f?.[key] || '').trim();
    return p && p === v.layout[key] ? v.mounted[p] : undefined; // only known for what's saved
  };

  async function save() {
    saving = true;
    try {
      v = await api.put('/host/storage/layout', Object.fromEntries(fields.map((x) => [x.key, (f[x.key] || '').trim()])));
      f = { ...v.layout };
      toast('Saved. Homestack, the mover and the alert jobs look here now.', 'success', 6000);
    } catch (e) {
      toast(e.message, 'error', 12000);
    }
    saving = false;
  }
</script>

<section class="card">
  <div class="card-header"><h2>Where the storage is</h2></div>
  {#if agentError}
    <div class="card-body dim small">{agentError}</div>
  {:else if !v}
    <div class="card-body"><div class="skeleton" style="height:160px"></div></div>
  {:else}
    <form class="card-body stack-v" onsubmit={(e) => { e.preventDefault(); save(); }}>
      <p class="intro">Where Homestack finds the shares, the cache and the disks. Changing it here doesn't mount or move anything: it has to match how they're mounted (<code>/etc/fstab</code> and the pools). The defaults are Unraid's paths.</p>
      {#if v.busy}<div class="note amber"><Icon name="alert" size={16} /><span>{v.busy}</span></div>{/if}

      <div class="grid">
        {#each fields as x (x.key)}
          {@const m = x.view ? mounted(x.key) : undefined}
          <label class="field">
            <span class="label">{x.label}
              {#if m}<span class="ok"><Icon name="check" size={12} /> {m}</span>
              {:else if m === ''}<span class="warn">not mounted</span>{/if}
            </span>
            <input class="mono" bind:value={f[x.key]} spellcheck="false" autocapitalize="off" placeholder={x.key === 'appdata' ? '(none)' : v.default[x.key]} />
            <span class="hint">
              {#if x.prefix}
                {x.hint} {x.prefix === 'disk' ? 'Disk 1' : 'Parity 1'} is at <code>{(f[x.key] || '').trim()}1</code>{#if x.prefix === 'disk'}, in a pool called <code>{base((f[x.key] || '').trim())}1</code>{/if}.
                {#if off(x.key).length}<span class="warn">snapraid.conf has {off(x.key).join(', ')}, which doesn't match.</span>
                {:else if (x.prefix === 'disk' ? v.disks : v.parity).length}<span class="ok">Matches the {(x.prefix === 'disk' ? v.disks : v.parity).length} in snapraid.conf.</span>{/if}
              {:else if x.key === 'owner'}
                User and group ID that owns the shares' files: new share folders, New permissions in Files, and the apps you install. Now {v.ownerName}.
              {:else if x.key === 'snapraid'}
                Where the data and parity disks are listed.
              {:else}
                {x.hint}
              {/if}
            </span>
          </label>
        {/each}
      </div>

      <div class="row">
        <button class="primary" type="submit" disabled={saving || !changed || !!v.busy}>{#if saving}<Icon name="loader" size={14} class="spin" />{/if} Save</button>
        {#if changed}<button type="button" class="ghost" onclick={() => (f = { ...v.layout })}>Undo changes</button>{/if}
        {#if !isDefault}<button type="button" class="ghost" onclick={() => (f = { ...v.default })}>Use Unraid's paths</button>{/if}
      </div>
      <span class="hint">Before saving, Homestack checks each changed place is mounted (the shares and the array as mergerfs) and that snapraid.conf's disks match, so the mover can never be pointed at an empty folder on the boot drive.</span>
    </form>
  {/if}
</section>

<style>
  .intro { margin: 0; font-size: 13.5px; line-height: 1.55; color: var(--text-2); }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 300px), 1fr)); gap: 16px 20px; }
  .field { display: grid; gap: 6px; font-size: 13px; align-content: start; min-width: 0; }
  .label { display: flex; gap: 8px; align-items: center; font-weight: 500; }
  .ok { display: inline-flex; gap: 4px; align-items: center; color: var(--green-text); font-weight: 400; }
  .warn { color: var(--amber-text); font-weight: 400; }
  .hint { color: var(--text-2); font-size: 12.5px; line-height: 1.5; }
  .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; border-radius: 10px; font-size: 13px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note :global(svg) { flex: none; margin-top: 2px; }
</style>
