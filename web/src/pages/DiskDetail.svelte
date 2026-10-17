<script>
  // One disk's SMART details, like Unraid's disk page: every attribute, the
  // self-test log and what the disk says it is. Read when the page opens and
  // on demand, not polled: a sleeping disk stays asleep unless you wake it.
  import { api } from '../lib/api.js';
  import { navigate } from '../lib/router.svelte.js';
  import { bytes, dateTime, duration } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';

  let { name, tab = 'attributes' } = $props();

  let d = $state(null);
  let error = $state('');
  let notFound = $state(false);
  let busy = $state(false);

  async function load(wake = false) {
    busy = true;
    try {
      d = await api.get(`/host/disks/smart?name=${encodeURIComponent(name)}${wake ? '&wake=1' : ''}`);
      error = '';
    } catch (e) {
      // The agent's own 404 names the disk; an agent from before this page
      // has no such route at all.
      if (e.status === 404 && e.message.startsWith("There's no disk")) notFound = true;
      error = e.status === 404 && !notFound
        ? "The host agent is older than this page. Update it: run deploy/debian/agent/install.sh from Homestack's source folder, with sudo."
        : e.message;
    }
    busy = false;
  }
  $effect(() => {
    name;
    d = null;
    notFound = false;
    load();
  });

  const tabs = [
    { id: 'attributes', label: 'Attributes', icon: 'activity' },
    { id: 'self-tests', label: 'Self-tests', icon: 'wrench' },
    { id: 'identity', label: 'Identity', icon: 'info' },
  ];
  const tabHref = (t) => `/storage/disks/${encodeURIComponent(name)}${t === 'attributes' ? '' : `/${t}`}`;

  // Raw_Read_Error_Rate -> Raw read error rate, keeping UDMA, LBAs, 1GiB…
  // smartctl shortens count to Ct (Reallocated_Sector_Ct). It calls an
  // attribute it has no name for Unknown_Attribute (or Unkn_CrucialMicron_Attr
  // and the like, when nobody outside the maker knows what it means).
  const unknown = (n) => /^Unkn/.test(n);
  const pretty = (n) => (unknown(n) ? 'Unknown' : n.split('_').map((w) => (w === 'Ct' ? 'count' : /^[A-Z0-9]{2,}s?$|\d/.test(w) ? w : w.toLowerCase())).join(' ').replace(/^./, (c) => c.toUpperCase()));
  const nameHint = (a) => (!unknown(a.name) ? a.name
    : d?.inDatabase ? `Not documented by the maker (smartctl: ${a.name})`
    : `smartctl has no name for it: this disk isn't in its drive database (${a.name})`);
  const hoursText = (h) => {
    const y = Math.floor(h / 8766), m = Math.floor((h % 8766) / 730.5), days = Math.floor(((h % 8766) % 730.5) / 24);
    return [y && `${y}y`, m && `${m}m`, `${days}d`].filter(Boolean).join(' ');
  };
  const num = (n) => (n ?? 0).toLocaleString();

  // Counts that should stay at zero: any at all is worth a look. By number
  // where it means the same on every disk; by smartctl's name where it
  // doesn't (170 is grown bad blocks on a WD SSD, the spare blocks on a
  // Crucial one).
  const watched = { 5: 'reallocated sectors', 187: 'reported uncorrectable errors', 197: 'sectors waiting to be reallocated', 198: 'uncorrectable sectors', 199: 'cable (CRC) errors' };
  const watchedNames = {
    Grown_Bad_Blocks: 'flash blocks that went bad in use', Grown_Failing_Block_Ct: 'flash blocks that went bad in use',
    Program_Fail_Count: 'failed flash writes', Program_Fail_Cnt_Total: 'failed flash writes', Program_Fail_Count_Chip: 'failed flash writes',
    Erase_Fail_Count: 'failed flash erases', Erase_Fail_Count_Total: 'failed flash erases', Erase_Fail_Count_Chip: 'failed flash erases',
  };
  const watch = (a) => watched[a.id] || watchedNames[a.name];
  const level = (a) => (a.whenFailed === 'now' ? 'bad' : a.whenFailed === 'past' || (watch(a) && a.raw > 0) ? 'warn' : '');
  const raw = (a) => (a.id === 9 && /^\d+$/.test(a.rawText) ? `${a.rawText} (${hoursText(a.raw)})` : a.rawText);

  // What's worth knowing first, worst first.
  const concerns = $derived.by(() => {
    if (!d || d.asleep) return [];
    const out = [];
    for (const a of d.attributes) {
      if (a.whenFailed === 'now') out.push(['bad', `${pretty(a.name)} is at or below its threshold (${a.value}, threshold ${a.threshold}): the disk expects to fail.`]);
    }
    if (d.passed === false) out.push(['bad', 'The disk reports that SMART has failed. Copy what matters off it and replace it.']);
    const h = d.nvme;
    if (h?.criticalWarning) out.push(['bad', `The disk reports a critical warning (0x${h.criticalWarning.toString(16)}).`]);
    if (h && h.availableSpare < h.spareThreshold) out.push(['bad', `Spare blocks are down to ${h.availableSpare}%, below the ${h.spareThreshold}% the disk warns at.`]);
    if (h?.mediaErrors) out.push(['warn', `${num(h.mediaErrors)} media errors (data the disk couldn't read back).`]);
    for (const a of d.attributes) {
      if (watch(a) && a.raw > 0) out.push(['warn', `${num(a.raw)} ${watch(a)} (${pretty(a.name)}).`]);
      else if (a.whenFailed === 'past') out.push(['warn', `${pretty(a.name)} fell below its threshold in the past.`]);
    }
    if (d.errorCount) out.push(['warn', `${num(d.errorCount)} error${d.errorCount === 1 ? '' : 's'} in the disk's error log.`]);
    const last = d.selfTests[0];
    if (last && last.passed === false) out.push(['warn', `The last self-test failed: ${last.status}.`]);
    return out;
  });

  const health = $derived(!d || d.asleep ? null : d.passed === true ? ['green', 'SMART passed'] : d.passed === false ? ['red', 'SMART failing'] : ['', 'SMART unknown']);

  const nvmeRows = $derived.by(() => {
    const h = d?.nvme;
    if (!h) return [];
    return [
      ['Critical warning', h.criticalWarning ? `0x${h.criticalWarning.toString(16)}` : 'None', h.criticalWarning ? 'bad' : ''],
      ['Temperature', `${h.temp}°C`],
      ['Available spare', `${h.availableSpare}% (warns below ${h.spareThreshold}%)`, h.availableSpare < h.spareThreshold ? 'bad' : ''],
      ['Percentage used', `${h.percentageUsed}% of its rated endurance`, h.percentageUsed >= 80 ? 'warn' : ''],
      ['Data read', bytes(h.dataUnitsRead * 512000)],
      ['Data written', bytes(h.dataUnitsWritten * 512000)],
      ['Host read commands', num(h.hostReads)],
      ['Host write commands', num(h.hostWrites)],
      ['Controller busy time', duration(h.busyMinutes * 60)],
      ['Power cycles', num(h.powerCycles)],
      ['Power on hours', `${num(h.powerOnHours)} (${hoursText(h.powerOnHours)})`],
      ['Unsafe shutdowns', num(h.unsafeShutdowns)],
      ['Media and data integrity errors', num(h.mediaErrors), h.mediaErrors ? 'warn' : ''],
      ['Error log entries', num(h.errorLogEntries)],
      ['Warning temperature time', `${num(h.warningTempMinutes)} min`],
      ['Critical temperature time', `${num(h.criticalTempMinutes)} min`],
    ];
  });

  const identity = $derived.by(() => {
    if (!d) return [];
    return [
      ['Model family', d.family],
      ['Model', d.model],
      ['Serial number', d.serial],
      ['WWN', d.wwn],
      ['Firmware', d.firmware],
      ['Capacity', d.capacity ? `${bytes(d.capacity)} (${num(d.capacity)} bytes)` : ''],
      ['Sector size', d.logicalBlock ? (d.physicalBlock && d.physicalBlock !== d.logicalBlock ? `${d.logicalBlock} bytes logical, ${d.physicalBlock} physical` : `${d.logicalBlock} bytes`) : ''],
      ['Rotation', d.rpm == null ? '' : d.rpm ? `${num(d.rpm)} rpm` : 'Solid state'],
      ['Form factor', d.formFactor],
      ['Interface', d.interface],
      ['Standard', d.standard],
      ['Protocol', d.protocol],
      ['In smartctl’s database', d.protocol === 'ATA' ? (d.inDatabase ? 'Yes' : 'No, so some attributes show as Unknown. The agent installer updates the database (sudo update-smart-drivedb does it by hand).') : ''],
      ['Read', dateTime(d.read)],
    ].filter(([, v]) => v);
  });
</script>

<div class="page">
  <a class="back" href="/storage"><Icon name="arrow-left" size={14} /> Storage</a>

  {#if notFound}
    <div class="card empty"><h3>Disk not found</h3><p>There's no disk called {name}.</p><a class="btn" href="/storage">Back to Storage</a></div>
  {:else}
    <div class="page-header">
      <div>
        <div class="row wrap" style="gap:10px">
          <h1>{d?.label || name}</h1>
          {#if health}<span class="badge {health[0]}">{health[1]}</span>{/if}
        </div>
        <p class="sub">/dev/{name}{d?.model ? ` · ${d.model}` : ''}{d?.serial ? ` · ${d.serial}` : ''}</p>
      </div>
      <button onclick={() => load()} disabled={busy}><Icon name="refresh" size={15} class={busy ? 'spin' : ''} /> Read again</button>
    </div>

    {#if error && !d}
      <div class="card note red"><Icon name="alert" size={16} /> {error}</div>
    {:else if !d}
      <div class="skeleton" style="height:320px"></div>
    {:else if d.asleep}
      <div class="card empty">
        <Icon name="moon" size={22} />
        <h3>{d.label || name} is asleep</h3>
        <p>It's spun down, so its SMART data wasn't read. Reading it spins the disk up.</p>
        <button class="primary" onclick={() => load(true)} disabled={busy}>{#if busy}<Icon name="loader" size={14} class="spin" />{/if} Spin up and read</button>
      </div>
    {:else}
      {#if error}<div class="note red"><Icon name="alert" size={16} /> {error}</div>{/if}
      {#each d.messages || [] as m}<div class="note"><Icon name="info" size={16} /> smartctl: {m}</div>{/each}
      {#if concerns.length}
        <div class="note {concerns.some((c) => c[0] === 'bad') ? 'red' : 'amber'}">
          <Icon name="alert" size={16} />
          <ul>{#each concerns as [, text]}<li>{text}</li>{/each}</ul>
        </div>
      {/if}

      <div class="tiles">
        <div class="card tile"><span><Icon name="thermometer" size={14} /> Temperature</span><strong>{d.temp != null ? `${d.temp}°C` : '–'}</strong></div>
        <div class="card tile"><span><Icon name="clock" size={14} /> Powered on</span><strong>{d.powerOnHours != null ? hoursText(d.powerOnHours) : '–'}</strong>{#if d.powerOnHours != null}<small>{num(d.powerOnHours)} hours</small>{/if}</div>
        <div class="card tile"><span><Icon name="power" size={14} /> Power cycles</span><strong>{d.powerCycles != null ? num(d.powerCycles) : '–'}</strong></div>
        <div class="card tile"><span><Icon name="disk" size={14} /> Capacity</span><strong>{d.capacity ? bytes(d.capacity) : '–'}</strong>{#if d.rpm != null}<small>{d.rpm ? `${num(d.rpm)} rpm` : 'Solid state'}</small>{/if}</div>
      </div>

      <nav class="tabs">
        {#each tabs as t}
          <a href={tabHref(t.id)} class:active={tab === t.id} onclick={(e) => { e.preventDefault(); navigate(tabHref(t.id), { replace: true }); }}>
            <Icon name={t.icon} size={15} /> {t.label}{#if t.id === 'self-tests' && d.selfTests.length}<span class="count">{d.selfTests.length}</span>{/if}
          </a>
        {/each}
      </nav>

      {#if tab === 'self-tests'}
        {#if d.selfTestNow}<div class="note"><Icon name="loader" size={16} class="spin" /> A self-test is running: {d.selfTestNow}.</div>{/if}
        <section class="card">
          {#if d.selfTests.length}
            <div class="table-wrap">
              <table class="table">
                <thead><tr><th class="n">#</th><th>Test</th><th>Result</th><th class="n">At hour</th><th class="hide-sm" title="Counted in the disk's power-on hours, so only roughly">When</th><th class="hide-sm">First error (LBA)</th></tr></thead>
                <tbody>
                  {#each d.selfTests as t, i}
                    <tr class={t.passed === false ? 'bad' : ''}>
                      <td class="n dim">{i + 1}</td>
                      <td>{t.type}</td>
                      <td><span class="badge {t.passed === true ? 'green' : t.passed === false ? 'red' : ''}">{t.status}</span></td>
                      <td class="n">{num(t.hours)}</td>
                      <td class="hide-sm dim nowrap">{d.powerOnHours != null && d.powerOnHours >= t.hours ? `≈ ${hoursText(d.powerOnHours - t.hours)} ago` : '–'}</td>
                      <td class="hide-sm mono">{t.lba ?? '–'}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {:else if d.selfTestNote}
            <div class="card-body dim small">{d.selfTestNote}</div>
          {:else}
            <div class="card-body dim small">No self-tests in the disk's log. To run a short one (a couple of minutes, the disk stays usable): <code>sudo smartctl -t short /dev/{name}</code></div>
          {/if}
        </section>
      {:else if tab === 'identity'}
        <section class="card">
          <dl class="kv">
            {#each identity as [k, v]}<dt>{k}</dt><dd>{v}</dd>{/each}
          </dl>
          {#if d.notes?.length}
            <div class="card-body dim small notes">Optional logs this disk doesn't offer (nothing to worry about):{#each d.notes as n}<div>smartctl: {n}</div>{/each}</div>
          {/if}
        </section>
      {:else if d.nvme}
        <section class="card">
          <div class="card-header"><h2>Health information</h2><span class="dim small">NVMe disks report this log in place of attributes</span></div>
          <div class="table-wrap">
            <table class="table">
              <tbody>
                {#each nvmeRows as [k, v, lvl]}<tr class={lvl}><td class="dim k">{k}</td><td class="v">{v}</td></tr>{/each}
              </tbody>
            </table>
          </div>
        </section>
      {:else if d.attributes.length}
        <section class="card">
          <div class="table-wrap">
            <table class="table attrs">
              <thead>
                <tr>
                  <th class="n">#</th><th>Attribute</th><th class="hide-sm">Flag</th>
                  <th class="n" title="Normalised: higher is better">Value</th><th class="n">Worst</th><th class="n" title="At or below this, the attribute has failed">Threshold</th>
                  <th class="hide-sm" title="Pre-fail: failing it predicts the disk failing. Old age: wear, expected over time">Type</th>
                  <th class="hide-sm" title="Always: kept up to date. Offline: only by the disk's own offline scans">Updated</th>
                  <th>Failed</th><th>Raw value</th>
                </tr>
              </thead>
              <tbody>
                {#each d.attributes as a}
                  <tr class={level(a)}>
                    <td class="n dim">{a.id}</td>
                    <td class="nowrap" class:dim={unknown(a.name)} title={nameHint(a)}>{pretty(a.name)}</td>
                    <td class="hide-sm mono dim">0x{a.flags.toString(16).padStart(4, '0')}</td>
                    <td class="n">{a.value}</td>
                    <td class="n">{a.worst}</td>
                    <td class="n">{a.threshold}</td>
                    <td class="hide-sm nowrap">{a.preFail ? 'Pre-fail' : 'Old age'}</td>
                    <td class="hide-sm">{a.online ? 'Always' : 'Offline'}</td>
                    <td class="nowrap">{a.whenFailed === 'now' ? 'Failing now' : a.whenFailed === 'past' ? 'In the past' : 'Never'}</td>
                    <td class="v nowrap">{raw(a)}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </section>
      {:else}
        <div class="card empty"><p>This disk doesn't report SMART attributes{d.protocol ? ` (${d.protocol})` : ''}. Its identity is under Identity.</p></div>
      {/if}
    {/if}
  {/if}
</div>

<style>
  .back { display: inline-flex; align-items: center; gap: 5px; color: var(--text-2); font-size: 13px; margin-bottom: 12px; }
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .small { font-size: 12.5px; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 12px; font-size: 13.5px; background: var(--surface-2); color: var(--text-2); }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note.red { background: var(--red-soft); color: var(--red-text); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .note ul { margin: 0; padding-left: 16px; display: grid; gap: 3px; }
  .empty :global(svg) { color: var(--text-3); margin-bottom: 8px; }
  .tiles { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 16px; }
  .tile { padding: 12px 16px; display: grid; gap: 4px; align-content: start; }
  .tile span { display: flex; align-items: center; gap: 6px; font-size: 12.5px; color: var(--text-2); }
  .tile strong { font-size: 17px; font-weight: 600; font-variant-numeric: tabular-nums; }
  .tile small { font-size: 12px; color: var(--text-3); }
  .count { font-size: 11px; color: var(--text-3); margin-left: 2px; }
  .table td.n, .table th.n { text-align: right; font-variant-numeric: tabular-nums; width: 1%; white-space: nowrap; }
  .attrs td { padding-top: 7px; padding-bottom: 7px; }
  .table td.v { font-variant-numeric: tabular-nums; }
  .table td.k { width: 280px; }
  tr.warn td { color: var(--amber-text); }
  tr.bad td { color: var(--red-text); font-weight: 500; }
  .kv { display: grid; grid-template-columns: max-content 1fr; gap: 9px 18px; padding: 14px 18px; margin: 0; font-size: 13.5px; }
  .kv dt { color: var(--text-2); }
  .kv dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
  .notes { border-top: 1px solid var(--border); display: grid; gap: 3px; }
  @media (max-width: 640px) { .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
