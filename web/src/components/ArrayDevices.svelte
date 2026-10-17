<script>
  // Every disk in one table, like Unraid's Main tab: the parity and data
  // slots (with a dropdown of spare disks on the empty one at the end of
  // each), the cache, then the other disks. Each row has its usage, reads
  // and writes, temperature, health and scrub, and a menu for the rest.
  // The agent checks everything again when a job starts; the dialogs explain
  // what it will do, and what stops it, first.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import { bytes, rate, ago } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from './Modal.svelte';
  import Menu from './Menu.svelte';

  // act runs a storage action (the Storage page's, so it refreshes too).
  let { disks = [], pools = [], cacheFS = null, refreshKey = '', act = () => {}, busy: actBusy = false } = $props();

  let v = $state(null);
  let error = $state('');
  let busy = $state(false);
  let dlg = $state(null); // { action, slot, parity, device }
  let typed = $state('');
  let folders = $state(true);
  let verify = $state(false);

  async function load() {
    try {
      v = await api.get('/host/array');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(() => {
    const t = setInterval(load, 30000);
    return () => clearInterval(t);
  });
  // Again when a disk job starts, moves on or finishes.
  $effect(() => {
    refreshKey;
    load();
  });

  const diskByName = (n) => disks.find((d) => d.name === n);
  const poolOf = (name) => pools.find((p) => p.name === name);
  const cache = $derived(disks.filter((d) => d.role === 'cache'));
  // Disks in no slot: the boot drive and unassigned ones.
  const others = $derived.by(() => {
    const used = new Set([...(v?.parity || []).map((p) => p.devName), ...(v?.data || []).map((d) => d.devName), ...cache.map((c) => c.name)]);
    return disks.filter((d) => !used.has(d.name));
  });
  const pct = (a, b) => (b ? Math.min(100, (a / b) * 100) : 0);
  const level = (p) => (p >= 90 ? 'red' : p >= 80 ? 'amber' : '');
  const io = (d, key) => (d && !d.asleep && d[key] >= 1 ? rate(d[key]) : '–');
  const spinnable = (d) => d && d.rotational && d.transport === 'sata' && d.role !== 'system';
  // SMART counters worth a look, in words.
  const warnings = (d) => (!d ? [] : [
    d.pending ? `${d.pending} unreadable` : '', d.offline ? `${d.offline} uncorrectable` : '', d.reallocated ? `${d.reallocated} replaced` : '',
    d.crc ? `${d.crc} cable error${d.crc === 1 ? '' : 's'}` : '', d.mediaErrors ? `${d.mediaErrors} media errors` : '', d.wearUsed != null && d.wearUsed >= 80 ? `${d.wearUsed}% worn` : '',
  ].filter(Boolean));
  // "scrub repaired 0B in 10:22:03 with 0 errors on Sun Oct 11 03:10:01 2026"
  function scrubText(p) {
    if (!p?.scan) return 'Never scrubbed';
    const m = p.scan.match(/with (\d+) errors on (.+)$/);
    const when = m ? new Date(m[2]) : null;
    if (!m || isNaN(when)) return p.scan;
    return `Scrubbed ${ago(when.toISOString())} · ${m[1] === '0' ? 'no errors' : `${m[1]} errors`}`;
  }
  // The row menu: SMART details and spin-down for any disk, plus extra.
  function menu(devName, extra = []) {
    const d = diskByName(devName);
    return [
      devName ? { label: 'SMART details', icon: 'activity', onclick: () => navigate(`/storage/disks/${encodeURIComponent(devName)}`) } : null,
      spinnable(d) && !d.asleep ? { label: 'Spin down', icon: 'moon', hint: 'It spins up again when it\'s read or written', disabled: actBusy, onclick: () => act({ action: 'disk-spindown', disk: d.name }) } : null,
      ...extra,
    ].filter(Boolean);
  }
  function scrubItems(p) {
    if (!p) return [];
    const go = (action) => () => act({ action, pool: p.name });
    if (p.scanState === 'scrubbing') return [{ label: 'Pause scrub', icon: 'pause', onclick: go('pool-scrub-pause') }];
    if (p.scanState === 'paused') return [{ label: 'Resume scrub', icon: 'play', onclick: go('pool-scrub') }, { label: 'Cancel scrub', icon: 'x', onclick: go('pool-scrub-stop') }];
    if (p.scanState === 'resilvering') return [];
    return [{ label: 'Scrub', icon: 'check', hint: 'ZFS reads every block and checks its checksum', disabled: actBusy || (!p.mounted && p.health !== 'ONLINE'), onclick: go('pool-scrub') }];
  }
  const blocked = $derived(v?.blocked || '');
  const stateText = { ONLINE: 'Working', MISSING: 'Missing', IMPORTABLE: 'Not imported', FAULTED: 'Faulted', UNAVAIL: 'Unavailable', DEGRADED: 'Degraded' };

  // Rough times, at the speeds these disks manage.
  // SnapRAID reads the data disks side by side, so a full pass takes about
  // as long as reading the fullest one.
  const fullest = $derived(Math.max(0, ...(v?.data || []).map((d) => d.used || 0)));
  function hours(bytesToRead, rate) {
    const h = bytesToRead / rate / 3600;
    return h < 1 ? 'under an hour' : h < 36 ? `about ${Math.round(h)} hours` : `about ${Math.round(h / 24)} days`;
  }

  function open(action, slot, parity) {
    dlg = { action, slot, parity, device: '' };
    typed = '';
    folders = true;
    verify = false;
  }
  function pick(action, slot, e) {
    const device = e.target.value;
    e.target.value = '';
    if (!device) return;
    open(action, slot);
    dlg.device = device;
  }
  const cand = $derived(v?.candidates.find((c) => c.device === dlg?.device));
  const why = $derived(!cand || !dlg ? '' : dlg.action === 'add-data' ? cand.notData : dlg.action === 'add-parity' ? cand.notParity : cand.notSwap?.[dlg.parity?.name] || '');
  const swapping = $derived(dlg?.action === 'replace-parity' || dlg?.action === 'rebuild-parity');
  const optLabel = (c, reason) => `${c.name} · ${c.model || 'Disk'} · ${c.serial} · ${bytes(c.size, 1)}${c.contents && c.contents !== 'Empty' ? ` · ${c.contents}` : ''}${reason ? ' (not suitable)' : ''}`;

  async function start() {
    busy = true;
    try {
      await api.post('/host/array', { action: dlg.action, device: dlg.device, confirm: typed.trim(), parity: dlg.parity?.name, folders, verify });
      toast('Started. Progress is on the next page, and in your notifications.', 'success', 5000);
      dlg = null;
      navigate('/storage/replace');
    } catch (e) {
      toast(e.message, 'error', 12000);
      load();
    }
    busy = false;
  }
</script>

{#snippet diskCell(devName, model, serial, missing, slot)}
  {@const d = diskByName(devName)}
  <div class="slot-sm">{slot}</div>
  {#if devName}<a href="/storage/disks/{encodeURIComponent(devName)}">{model || d?.model || 'Disk'}</a><div class="dim small mono">{serial || d?.serial} · {devName}{d?.powerOnHours ? ` · ${(d.powerOnHours / 8766).toFixed(1)} y` : ''}</div>
  {:else}<span class="dim">{missing}</span>{/if}
{/snippet}
{#snippet usedCell(used, size, text)}
  {#if size}
    <div class="meter thin {level(pct(used, size))}" title="{Math.round(pct(used, size))}% used"><div style="width:{pct(used, size)}%"></div></div>
    <div class="dim small">{text}</div>
  {:else}<span class="dim">–</span>{/if}
{/snippet}
{#snippet ioCells(devName)}
  {@const d = diskByName(devName)}
  <td class="hide-sm nowrap num" title={d ? `${bytes(d.readBytes)} read since the server started` : ''}>{io(d, 'readBps')}</td>
  <td class="hide-sm nowrap num" title={d ? `${bytes(d.writtenBytes)} written since the server started` : ''}>{io(d, 'writeBps')}</td>
  <td class="hide-sm nowrap">{#if d?.temp != null}{d.temp}°C{:else if d?.asleep}<span class="dim sleep" title="Spun down: it wakes when it's read or written"><Icon name="moon" size={12} /> asleep</span>{:else}<span class="dim">–</span>{/if}</td>
{/snippet}
{#snippet health(devName)}
  {@const w = warnings(diskByName(devName))}
  {#if w.length}<div class="small amber-t">{w.join(', ')}</div>{/if}
{/snippet}

<section class="card">
  <div class="card-header">
    <h2>Disks</h2>
    <span class="dim small hide-sm">Which disk is what. A disk's name opens its SMART details.</span>
    <span class="spacer"></span>
    <a class="btn sm ghost" href="/storage/replace"><Icon name="swap" size={13} /> Replace a disk</a>
  </div>
  {#if error}
    <div class="card-body dim small">{error}</div>
  {:else if !v}
    <div class="card-body"><div class="skeleton" style="height:160px"></div></div>
  {:else}
    {#if blocked}
      <div class="note"><Icon name="info" size={15} /> <span>Disks can't be added or moved right now: {blocked}</span></div>
    {/if}
    <div class="table-wrap">
      <table class="table slots">
        <thead><tr><th class="hide-sm">Slot</th><th>Disk</th><th class="hide-sm">Size</th><th class="hide-sm">Used</th><th class="hide-sm num">Read</th><th class="hide-sm num">Write</th><th class="hide-sm">Temp</th><th>State</th><th></th></tr></thead>
        <tbody>
          {#each v.parity as p (p.name)}
            {@const urgent = !p.mounted || p.failing}
            {@const replace = { label: 'Replace', icon: 'swap', disabled: !!blocked || !!v.parityBlocked, hint: v.parityBlocked || (p.mounted ? 'Move parity to a new (bigger) disk' : 'Rebuild it onto a new disk from the data disks'), onclick: () => open(p.mounted ? 'replace-parity' : 'rebuild-parity', null, p) }}
            <tr>
              <td class="slot hide-sm">{p.label}</td>
              <td>{@render diskCell(p.devName, p.model, p.serial, p.mounted ? 'Unknown disk' : 'Disk not found', p.label)}</td>
              <td class="hide-sm nowrap">{p.size ? bytes(p.size, 1) : '–'}</td>
              <td class="usedc hide-sm">{@render usedCell(p.used, p.size, `${bytes(p.used, 1)} of parity`)}</td>
              {@render ioCells(p.devName)}
              <td><span class="dot {urgent ? 'red' : 'green'}"></span> {!p.mounted ? 'Not mounted' : p.failing ? 'Failing SMART' : 'Working'}{@render health(p.devName)}</td>
              <td class="actions">
                {#if urgent}<button class="sm primary" onclick={replace.onclick} disabled={replace.disabled} title={replace.hint}>Replace</button>{/if}
                <Menu size="sm" items={menu(p.devName, urgent ? [] : [{ divider: true }, replace])} />
              </td>
            </tr>
          {/each}
          {#if v.nextParity || v.parityBlocked}
            <tr class="empty">
              <td class="slot hide-sm">{v.nextParity?.label || 'Parity'}</td>
              <td colspan="8">
                {#if v.parityBlocked || v.nextParity.blocked}
                  <span class="dim small">{v.parityBlocked || v.nextParity.blocked}</span>
                {:else}
                  <select onchange={(e) => pick('add-parity', v.nextParity, e)} disabled={!!blocked || !v.candidates.length} aria-label="Assign a disk to {v.nextParity.label}">
                    <option value="">{v.candidates.length ? `Add a parity disk…` : 'No spare disk connected'}</option>
                    {#each v.candidates as c (c.device || c.name)}<option value={c.device}>{optLabel(c, c.notParity)}</option>{/each}
                  </select>
                {/if}
              </td>
            </tr>
          {/if}
          {#each v.data as d (d.snap)}
            {@const pool = poolOf(d.pool)}
            {@const ok = d.state === 'ONLINE' && d.mounted && !d.smartFailing}
            {@const replaceTo = d.method ? `/storage/replace?disk=${encodeURIComponent(d.pool)}` : ''}
            {@const plain = d.fsType && d.fsType !== 'zfs'}
            <tr>
              <td class="slot hide-sm">Disk {d.pool.replace(/^\D+/, '')}<div class="dim small">{d.snap}</div></td>
              <td>{@render diskCell(d.devName, d.model, d.serial, d.model ? `Was ${d.model}` : 'Missing', `Disk ${d.pool.replace(/^\D+/, '')}`)}</td>
              <td class="hide-sm nowrap">{d.size ? bytes(d.size, 1) : '–'}</td>
              <td class="usedc hide-sm">{@render usedCell(pool?.alloc ?? d.used, pool?.size ?? d.poolSize ?? 0, `${bytes(pool?.alloc ?? d.used, 1)} used${plain ? ` · ${d.fsType}` : ''}`)}</td>
              {@render ioCells(d.devName)}
              <td>
                <span class="dot {ok ? 'green' : 'red'}"></span> {d.smartFailing ? 'Failing SMART' : d.state !== 'ONLINE' ? stateText[d.state] || d.state : !d.mounted ? 'Not mounted' : 'Working'}
                {#if plain && d.state === 'MISSING'}<div class="small dim">Check its cable and power, and its line in /etc/fstab. If the disk is dead, its menu says how to replace it: parity still has its files.</div>
                {:else if d.state === 'MISSING' || d.state === 'IMPORTABLE'}<div class="small dim">Check its cable and power. If the disk is dead, replace it: parity still has its files.</div>{/if}
                {@render health(d.devName)}
                {#if pool && (pool.scanState === 'scrubbing' || pool.scanState === 'resilvering' || pool.scanState === 'paused')}
                  <div class="small scan"><span>{pool.scanState === 'resilvering' ? 'Rebuilding' : pool.scanState === 'paused' ? 'Scrub paused' : 'Scrubbing'}{pool.scanPercent != null ? ` ${pool.scanPercent.toFixed(1)}%` : ''}{pool.scanEta ? ` · ${pool.scanEta} left` : ''}</span>
                    {#if pool.scanPercent != null}<div class="meter thin"><div style="width:{pool.scanPercent}%"></div></div>{/if}</div>
                {:else if pool}
                  <div class="small dim truncate" title={pool.scan}>{scrubText(pool)}</div>
                {/if}
              </td>
              <td class="actions">
                {#if !ok && replaceTo}<a class="btn sm primary" href={replaceTo}>Replace</a>{/if}
                <Menu size="sm" items={menu(d.devName, [...scrubItems(pool), ...(ok && replaceTo ? [{ divider: true }, { label: 'Replace', icon: 'swap', hint: 'Copy it to a new disk, or rebuild it from parity', onclick: () => navigate(replaceTo) }] : []),
                  ...(plain ? [{ divider: true }, { label: 'Replace', icon: 'swap', disabled: true, hint: d.why }] : [])])} />
              </td>
            </tr>
          {/each}
          {#if v.nextData}
            <tr class="empty">
              <td class="slot hide-sm">{v.nextData.label}</td>
              <td colspan="8">
                {#if v.nextData.blocked}
                  <span class="dim small">{v.nextData.blocked}</span>
                {:else}
                  <select onchange={(e) => pick('add-data', v.nextData, e)} disabled={!!blocked || !v.candidates.length} aria-label="Assign a disk to {v.nextData.label}">
                    <option value="">{v.candidates.length ? 'Add a data disk…' : 'No spare disk connected'}</option>
                    {#each v.candidates as c (c.device || c.name)}<option value={c.device}>{optLabel(c, c.notData)}</option>{/each}
                  </select>
                {/if}
              </td>
            </tr>
          {/if}
          {#each cache as c (c.name)}
            <tr>
              <td class="slot hide-sm">Cache</td>
              <td>{@render diskCell(c.name, c.model, c.serial, '', 'Cache')}</td>
              <td class="hide-sm nowrap">{bytes(c.size, 1)}</td>
              <td class="usedc hide-sm">{@render usedCell(cacheFS?.used, cacheFS?.size, cacheFS ? `${bytes(cacheFS.used, 1)} used · ${cacheFS.fstype}` : '')}</td>
              {@render ioCells(c.name)}
              <td><span class="dot {c.health === 'failed' ? 'red' : 'green'}"></span> {c.health === 'failed' ? 'Failing SMART' : 'Working'}{@render health(c.name)}</td>
              <td class="actions"><Menu size="sm" items={menu(c.name)} /></td>
            </tr>
          {/each}
          {#if others.length}
            <tr class="group"><td colspan="9">Other disks</td></tr>
            {#each others as o (o.name)}
              <tr>
                <td class="slot hide-sm">{o.role === 'system' ? 'Boot' : 'Unassigned'}</td>
                <td>{@render diskCell(o.name, o.model, o.serial, '', o.role === 'system' ? 'Boot' : 'Unassigned')}</td>
                <td class="hide-sm nowrap">{bytes(o.size, 1)}</td>
                <td class="hide-sm"><span class="dim">–</span></td>
                {@render ioCells(o.name)}
                <td><span class="dot {o.health === 'failed' ? 'red' : o.health === 'ok' ? 'green' : 'gray'}"></span> {o.health === 'failed' ? 'Failing SMART' : o.health === 'ok' ? 'Healthy' : o.asleep ? 'Asleep' : 'Unknown'}{@render health(o.name)}</td>
                <td class="actions"><Menu size="sm" items={menu(o.name)} /></td>
              </tr>
            {/each}
          {/if}
        </tbody>
      </table>
    </div>
  {/if}
</section>

{#if dlg}
  {@const p = dlg.parity}
  <Modal title={dlg.action === 'add-data' ? `Add a disk as ${dlg.slot.label}` : dlg.action === 'add-parity' ? `Add a disk as ${dlg.slot.label}` : dlg.action === 'rebuild-parity' ? `Replace ${p.label}'s disk` : `Move ${p.label} to a new disk`} width={600} onclose={() => (dlg = null)}>
    <div class="stack-v">
      {#if dlg.action === 'rebuild-parity'}
        <div class="note amber"><Icon name="alert" size={15} /> <span>{p.label} isn't working, so its parity can't be copied: it's rebuilt from the data disks onto the new one. {v.parity.length === 1 ? 'Until that finishes the array has no parity, so a data disk failing meanwhile would lose its files.' : 'Until that finishes the array has one parity level fewer.'} Every data disk is healthy right now.</span></div>
      {/if}
      {#if swapping}
        <label class="field">
          <span>The new disk</span>
          <select bind:value={dlg.device}>
            <option value="">Pick a spare disk…</option>
            {#each v.candidates as c (c.device || c.name)}<option value={c.device}>{optLabel(c, c.notSwap?.[p.name])}</option>{/each}
          </select>
        </label>
      {/if}

      {#if cand}
        <div class="disk">
          <Icon name="disk" size={18} />
          <div class="grow"><strong>{cand.model || 'Disk'}</strong><div class="dim small mono">{cand.serial} · {cand.name} · {bytes(cand.size, 1)}{cand.rotational ? '' : ' · SSD'}</div></div>
          <span class="dim small">{cand.contents}</span>
        </div>
        {#if why}
          <div class="note red"><Icon name="alert" size={15} /> <span>It can't be used for this: {why}</span></div>
        {:else}
          {#if cand.warn}<div class="note amber"><Icon name="alert" size={15} /> <span><strong>{cand.warn}</strong> ({cand.contents}).</span></div>{/if}

          <div>
            <div class="what">What happens</div>
            <ol class="plan">
              {#if dlg.action === 'add-data'}
                <li>The disk is erased and gets a ZFS pool, <code>{dlg.slot.pool}</code> at <code>{dlg.slot.path}</code>, with the same settings as your other disks.</li>
                <li>
                  <label class="check"><input type="checkbox" bind:checked={folders} /> Create the share folders on it{#if v.folders.length}{' '}({v.folders.join(', ')}){/if}</label>
                  <div class="dim small">New files only go to a disk that has the share's folder. Without them it stays empty until you create folders on it.</div>
                </li>
                <li>It's added to SnapRAID as <code>{dlg.slot.name}</code> and to your shares (<code>{v.shares || '/mnt/user'}</code>), now and after a restart.</li>
                <li>Parity is updated. That's quick: the new disk is empty.</li>
              {:else if dlg.action === 'add-parity'}
                <li>The disk is erased, partitioned and formatted like {v.parity[0]?.label || 'the first parity disk'}, and mounted at <code>{dlg.slot.path}</code> (and in <code>/etc/fstab</code>).</li>
                <li>It's added to SnapRAID as <code>{dlg.slot.name}</code>.</li>
                <li>The new parity is built from every disk: {hours(fullest, 150e6)}. Your current parity keeps protecting the array meanwhile.</li>
                <li>Then up to {dlg.slot.level} disks can fail at once without losing data.</li>
              {:else if dlg.action === 'rebuild-parity'}
                <li>The disk is erased, partitioned and formatted {p.fsType}, like {p.label}'s was.</li>
                <li>It takes the old disk's place at <code>{p.mount}</code> (its line in <code>/etc/fstab</code>).</li>
                <li>{p.label} is rebuilt from the data disks (<code>snapraid -d {p.name} fix</code>, as SnapRAID's manual says): {hours(fullest, 150e6)}. If it's stopped or the server restarts, it carries on.</li>
                <li>
                  <label class="check"><input type="checkbox" bind:checked={verify} /> Then check the rebuilt parity</label>
                  <div class="dim small">Reads every disk again ({hours(fullest, 150e6)}), with the mover still paused. SnapRAID calls it optional; the scheduled parity check looks again anyway.</div>
                </li>
                <li>A parity sync, and the array is protected again.</li>
              {:else}
                <li>The disk is erased, partitioned and formatted {p.fsType}, like {p.label}'s.</li>
                <li>{p.label}'s files ({bytes(p.used, 1)}) are copied onto it: {hours(p.used, 180e6)}. If it's stopped or the server restarts, it carries on where it was.</li>
                <li>It takes the old disk's place at <code>{p.mount}</code> (and in <code>/etc/fstab</code>), and a parity sync confirms it.</li>
                <li>The old disk ({p.serial}) is then unassigned and keeps a copy of the parity until you reuse it.</li>
              {/if}
            </ol>
          </div>
          <p class="dim small">The array stays up. The mover and the parity jobs wait until it's finished. <code>/etc/fstab</code> and <code>snapraid.conf</code> are backed up first and only changed by versions that check out. {dlg.action === 'rebuild-parity' ? 'Cancel is possible until the new disk takes over; after that only finishing brings parity back.' : `Cancel undoes it${dlg.action === 'add-data' ? ', as long as no files have been written to the new disk yet' : dlg.action === 'replace-parity' ? ' until the new disk takes over' : ''}.`}</p>

          <label class="field">
            <span>Type the disk's serial number to confirm: <code>{cand.serial}</code></span>
            <input class="mono" bind:value={typed} autocapitalize="off" spellcheck="false" autocomplete="off" placeholder={cand.serial} />
          </label>
        {/if}
      {:else if !swapping}
        <p class="dim small">That disk isn't available any more.</p>
      {/if}
    </div>
    {#snippet footer()}
      <button class="ghost" onclick={() => (dlg = null)}>Cancel</button>
      <button class="primary" onclick={start} disabled={busy || !cand || !!why || typed.trim() !== cand.serial}>
        {#if busy}<Icon name="loader" size={14} class="spin" />{/if}
        {dlg.action === 'replace-parity' ? 'Move parity' : dlg.action === 'rebuild-parity' ? 'Rebuild parity' : dlg.action === 'add-parity' ? 'Add parity disk' : 'Add the disk'}
      </button>
    {/snippet}
  </Modal>
{/if}

<style>
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; margin: 12px 18px 0; border-radius: 10px; font-size: 13px; background: var(--surface-2); color: var(--text-2); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .stack-v .note { margin: 0; }
  .note.red { background: var(--red-soft); color: var(--red-text); }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .slots td { vertical-align: middle; }
  .slot { font-weight: 600; white-space: nowrap; width: 1%; }
  .slots a { color: var(--text); font-weight: 500; }
  tr.empty td { background: color-mix(in srgb, var(--surface-2) 45%, transparent); }
  tr.empty select { width: 100%; max-width: min(520px, 62vw); }
  .dot { margin-right: 4px; vertical-align: 1px; }
  .disk { display: flex; gap: 12px; align-items: center; padding: 12px 14px; border: 1px solid var(--border); border-radius: 10px; }
  .disk :global(svg) { flex: none; color: var(--text-2); }
  .grow { flex: 1; min-width: 0; }
  .what { font-size: 13px; font-weight: 600; margin-bottom: 6px; }
  .plan { margin: 0; padding-left: 20px; display: grid; gap: 8px; font-size: 13.5px; line-height: 1.5; }
  .check { display: flex; gap: 8px; align-items: center; cursor: pointer; }
  .check input { width: 16px; height: 16px; accent-color: var(--accent); }
  .field { display: grid; gap: 6px; font-size: 13px; }
  .small { font-size: 12.5px; }
  p { margin: 0; }
  .usedc { min-width: 110px; }
  .usedc .meter { margin-bottom: 4px; }
  .num { text-align: right; font-variant-numeric: tabular-nums; }
  .scan { display: grid; gap: 3px; margin-top: 2px; max-width: 220px; }
  .truncate { display: block; max-width: 240px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sleep { display: inline-flex; align-items: center; gap: 4px; }
  .amber-t { color: var(--amber-text); }
  .spacer { flex: 1; }
  tr.group td { background: var(--surface-2); color: var(--text-2); font-size: 12px; font-weight: 600; padding-top: 7px; padding-bottom: 7px; }
  .actions { white-space: nowrap; text-align: right; }
  .actions :global(.menu) { display: inline-block; vertical-align: middle; }
  .slot-sm { display: none; font-size: 12px; font-weight: 600; color: var(--text-2); }
  @media (max-width: 860px) {
    .slot-sm { display: block; }
    .scan, .truncate { max-width: 150px; }
  }
</style>
