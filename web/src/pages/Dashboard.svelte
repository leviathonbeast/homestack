<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { live, features } from '../lib/state.svelte.js';
  import { bytes, percent, duration, ago, rate, bitrate, linkSpeed } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import AppLauncher from '../components/AppLauncher.svelte';
  import Sparkline from '../components/Sparkline.svelte';

  let data = $state(null);
  let error = $state('');
  let vms = $state(null);
  let st = $state(null); // the host agent's status (storage), when it's running
  let topBy = $state('cpu');

  async function load() {
    if (features.vms) api.get('/vms').then((v) => (vms = v)).catch(() => {});
    try {
      data = await api.get('/dashboard');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  async function loadStorage() {
    if (!features.host) return;
    st = await api.get('/host/status').catch(() => null);
  }

  onMount(() => {
    const t = setInterval(load, 5000);
    const t2 = setInterval(loadStorage, 10000);
    return () => (clearInterval(t), clearInterval(t2));
  });
  $effect(() => {
    live.tick;
    load();
  });
  $effect(() => {
    if (features.host) loadStorage();
  });

  const h = $derived(data?.host);
  const c = $derived(data?.counts);
  const hist = $derived(data?.history || []);
  const managed = $derived((data?.stacks || []).filter((s) => !s.external));
  const withUpdates = $derived(managed.filter((s) => s.updateAvailable));
  const top = $derived(data?.top?.[topBy] || []);
  // Bars: CPU against one full core at least, memory against the biggest here.
  const cpuMax = $derived(Math.max(100, ...top.map((t) => t.cpu)));
  const memMax = $derived(Math.max(1, ...top.map((t) => t.mem)));

  const pct = (a, b) => (b ? Math.min(100, (a / b) * 100) : 0);
  const level = (p) => (p >= 90 ? 'red' : p >= 75 ? 'amber' : '');
  const tempLevel = (t) => (t >= 85 ? 'red' : t >= 72 ? 'amber' : '');
  const series = (key) => hist.map((p) => p[key] || 0);

  // Storage summary (host agent)
  const arrayPct = $derived(st ? pct(st.totals.used, st.totals.size) : 0);
  const cachePct = $derived(st?.cache ? pct(st.cache.used, st.cache.size) : 0);
  const awake = $derived((st?.disks || []).filter((d) => d.temp != null));
  const hottest = $derived(awake.reduce((m, d) => (d.temp > (m?.temp ?? -1) ? d : m), null));
  const failing = $derived((st?.disks || []).filter((d) => d.health === 'failed'));
  const storageErrors = $derived((st?.warnings || []).filter((w) => w.level === 'error'));
  const parityText = $derived.by(() => {
    const p = st?.parity;
    if (!p) return '';
    if (!p.installed) return 'Not installed';
    if (!p.configured) return 'Not set up';
    if (p.syncing) {
      const r = st.running?.action === 'parity-sync' ? st.running : null;
      return `${r?.paused ? 'Sync paused' : 'Syncing'}${r?.progress != null ? ` · ${r.progress}%` : '…'}`;
    }
    if (!p.synced) return 'No parity yet';
    return p.daysSinceSync === 0 ? 'Up to date' : `${p.daysSinceSync} day${p.daysSinceSync === 1 ? '' : 's'} behind`;
  });
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Dashboard</h1>
      {#if data}
        <p class="sub">{data.docker.name} · {data.docker.os} · up {duration(h.uptimeSec)}</p>
      {:else}
        <p class="sub">&nbsp;</p>
      {/if}
    </div>
  </div>

  {#if error}
    <div class="card body-pad error-card"><Icon name="alert" /> {error}</div>
  {/if}

  {#if st?.running}
    {@const j = st.running}
    <a class="card jobline" href="/storage">
      {#if j.paused}<Icon name="pause" size={15} />{:else}<Icon name="loader" size={15} class="spin" />{/if}
      <strong>{j.title}</strong>
      {#if j.progress != null}<div class="meter thin grow" class:amber={j.paused}><div style="width:{j.progress}%"></div></div><span>{j.progress}%</span>{:else}<span class="grow dim truncate">{j.step || ''}</span>{/if}
      {#if j.paused}<span class="amber-t">Paused</span>{:else if j.eta}<span class="dim hide-sm">{j.eta} left</span>{/if}
    </a>
  {/if}
  {#each storageErrors as w}
    <a class="note err" href="/storage"><Icon name="alert" size={15} /> {w.text}</a>
  {/each}

  <h2 class="section">Server</h2>
  <section class="grid4">
    <div class="card tile">
      <div class="label"><Icon name="cpu" size={15} /> CPU
        {#if h?.cpuTemp != null}<span class="temp {tempLevel(h.cpuTemp)}" title="CPU temperature"><Icon name="thermometer" size={12} />{Math.round(h.cpuTemp)}°C</span>{/if}
      </div>
      <div class="value">{data ? percent(h.cpuPercent) : '–'}</div>
      <Sparkline series={[{ values: series('cpu'), color: 'var(--accent)' }]} max={100} label="CPU use, last 15 minutes" />
      <div class="foot" title={h?.cpuModel}>{data ? `${h.cpuThreads || data.docker.cpus} threads · load ${h.load1.toFixed(2)}, ${h.load5.toFixed(2)}, ${h.load15.toFixed(2)}` : ''}</div>
    </div>
    <div class="card tile">
      <div class="label"><Icon name="memory" size={15} /> Memory</div>
      <div class="value">{data ? bytes(h.memUsed) : '–'} <small>{data ? `of ${bytes(h.memTotal, 0)}` : ''}</small></div>
      {#if data}
        <div class="stackbar" title="Programs {bytes(h.memUsed)} · ZFS cache {bytes(h.memArc)} · page cache {bytes(h.memCache)} · free {bytes(h.memFree)}">
          <div class="seg used" style="width:{pct(h.memUsed, h.memTotal)}%"></div>
          <div class="seg arc" style="width:{pct(h.memArc, h.memTotal)}%"></div>
          <div class="seg cache" style="width:{pct(h.memCache, h.memTotal)}%"></div>
        </div>
      {/if}
      <Sparkline series={[{ values: series('mem'), color: 'var(--accent)' }]} max={100} height={22} label="Memory use, last 15 minutes" />
      <div class="foot">{#if data}{h.memArc ? `ZFS cache ${bytes(h.memArc)} · ` : ''}cache {bytes(h.memCache, 0)} · {bytes(h.memFree, 0)} free{h.swapUsed ? ` · swap ${bytes(h.swapUsed)}` : ''}{/if}</div>
    </div>
    <div class="card tile">
      <div class="label"><Icon name="network" size={15} /> Network</div>
      <div class="value two"><span><span class="arrow in">↓</span>{data ? bitrate(h.netRx) : '–'}</span><span><span class="arrow out">↑</span>{data ? bitrate(h.netTx) : '–'}</span></div>
      <Sparkline series={[{ values: series('netRx'), color: 'var(--accent)' }, { values: series('netTx'), color: 'var(--green)' }]} label="Network traffic, last 15 minutes" />
      <div class="foot truncate">{(h?.ifaces || []).map((i) => `${i.name}${i.up ? (i.speed ? ` ${linkSpeed(i.speed)}` : '') : ' down'}`).join(' · ') || 'No network ports found'}</div>
    </div>
    <div class="card tile">
      <div class="label"><Icon name="disk" size={15} /> Disk activity</div>
      <div class="value two"><span><span class="arrow in">R</span>{data ? rate(h.diskRead) : '–'}</span><span><span class="arrow out">W</span>{data ? rate(h.diskWrite) : '–'}</span></div>
      <Sparkline series={[{ values: series('diskRead'), color: 'var(--accent)' }, { values: series('diskWrite'), color: 'var(--amber)' }]} label="Disk reads and writes, last 15 minutes" />
      <div class="foot">All physical disks · last 15 minutes</div>
    </div>
  </section>

  <section class="card sys-card">
    <div class="card-header"><h2>System</h2></div>
    {#if data}
      <dl class="sys">
        <div><dt>Host</dt><dd>{data.docker.name}</dd></div>
        <div><dt>Processor</dt><dd>{h.cpuModel || `${data.docker.cpus} CPUs`}{h.cpuModel ? ` · ${h.cpuThreads} threads` : ''}</dd></div>
        <div><dt>System</dt><dd>{data.docker.os} · {data.docker.arch}</dd></div>
        <div><dt>Kernel</dt><dd class="mono">{data.docker.kernel}</dd></div>
        <div><dt>Docker</dt><dd>{data.docker.version} · {data.docker.storageDriver}</dd></div>
        <div><dt>Uptime</dt><dd>{duration(h.uptimeSec)} · {h.procs} processes</dd></div>
        <div><dt>Swap</dt><dd>{h.swapTotal ? `${bytes(h.swapUsed)} of ${bytes(h.swapTotal)}` : 'None'}</dd></div>
        {#if h.memArcMax}<div><dt>ZFS cache</dt><dd>{bytes(h.memArc)} (up to {bytes(h.memArcMax, 0)}) · <a href="/settings/storage">change the limit</a></dd></div>{/if}
        {#each h.ifaces as i}
          <div><dt>{i.name}</dt><dd>{i.up ? `Up${i.speed ? ` · ${linkSpeed(i.speed)}b/s` : ''} · ↓ ${bitrate(i.rx)} ↑ ${bitrate(i.tx)}` : 'Down'}</dd></div>
        {/each}
        {#each h.disks as d}
          <div><dt>{d.label === 'Stacks' ? 'Stacks folder' : 'System disk'}</dt><dd>{bytes(d.total - d.used)} free of {bytes(d.total)}</dd></div>
        {/each}
        <div><dt>Homestack</dt><dd>v{data.homestack}</dd></div>
      </dl>
    {:else}
      <div class="card-body"><div class="skeleton" style="height:96px"></div></div>
    {/if}
  </section>

  <AppLauncher />

  {#if st}
    <section class="storage">
      <a class="card mini" href="/storage">
        <div class="label"><Icon name="database" size={14} /> Array</div>
        <div class="mv">{bytes(st.totals.used, 1)} <small>of {bytes(st.totals.size, 1)}</small></div>
        <div class="meter {level(arrayPct)}"><div style="width:{arrayPct}%"></div></div>
        <div class="foot">{bytes(st.totals.free, 1)} free · {st.totals.disks} disks</div>
      </a>
      {#if st.cache}
        <a class="card mini" href="/storage">
          <div class="label"><Icon name="disk" size={14} /> Cache</div>
          <div class="mv">{bytes(st.cache.used, 0)} <small>of {bytes(st.cache.size, 0)}</small></div>
          <div class="meter {level(cachePct)}"><div style="width:{cachePct}%"></div></div>
          <div class="foot">{bytes(st.cache.avail, 0)} free · {st.cache.fstype}</div>
        </a>
      {/if}
      <a class="card mini" href="/storage">
        <div class="label"><Icon name="shield" size={14} /> Parity</div>
        <div class="mv {!st.parity.installed || !st.parity.configured ? '' : st.parity.syncing ? 'amber-t' : st.parity.synced && st.parity.daysSinceSync === 0 ? 'green-t' : st.parity.daysSinceSync >= 3 || !st.parity.synced ? 'amber-t' : ''}">{parityText}</div>
        <div class="foot">{st.parity.lastSync ? `Synced ${ago(st.parity.lastSync)}` : ' '}{st.parity.lastCheck ? ` · checked ${ago(st.parity.lastCheck.finished)}` : ''}</div>
      </a>
      <a class="card mini" href="/storage">
        <div class="label"><Icon name="upgrade" size={14} /> Mover</div>
        <div class="mv {st.mover && !st.mover.ok ? 'red-t' : ''}">{st.mover ? (st.mover.ok ? 'Last run OK' : 'Last run failed') : 'Not run yet'}</div>
        <div class="foot" title={st.mover?.summary}>{st.mover ? `${ago(st.mover.finished)} · ${st.mover.summary}` : ' '}</div>
      </a>
      <a class="card mini" href="/storage">
        <div class="label"><Icon name="thermometer" size={14} /> Disks</div>
        <div class="mv {failing.length ? 'red-t' : hottest?.temp >= 50 ? 'red-t' : hottest?.temp >= 45 ? 'amber-t' : ''}">{failing.length ? `${failing.length} failing` : hottest ? `Hottest ${hottest.temp}°C` : 'All asleep'}</div>
        <div class="foot">{st.disks.length} disks{hottest ? ` · ${hottest.label}` : ''}{st.disks.length - awake.length ? ` · ${st.disks.length - awake.length} asleep` : ''}</div>
      </a>
      {#if st.ups}
        {@const u = st.ups}
        <a class="card mini" href="/storage">
          <div class="label"><Icon name="power" size={14} /> UPS</div>
          <div class="mv {u.error ? 'amber-t' : u.onBattery ? 'red-t' : 'green-t'}">{u.error ? 'No contact' : u.onBattery ? 'On battery' : 'On mains'}</div>
          <div class="foot">{u.charge != null ? `${Math.round(u.charge)}%` : ''}{u.runtime != null ? ` · ${Math.round(u.runtime / 60)} min` : ''}{u.load != null ? ` · ${Math.round(u.load)}% load` : ''}</div>
        </a>
      {/if}
    </section>
  {:else if h?.disks?.length}
    <section class="storage">
      {#each h.disks as d}
        <div class="card mini">
          <div class="label"><Icon name="disk" size={14} /> {d.label === 'Stacks' ? 'Stacks folder' : 'System disk'}</div>
          <div class="mv">{bytes(d.used)} <small>of {bytes(d.total)}</small></div>
          <div class="meter {level(pct(d.used, d.total))}"><div style="width:{pct(d.used, d.total)}%"></div></div>
          <div class="foot mono truncate" title={d.path}>{bytes(d.total - d.used)} free · {d.path}</div>
        </div>
      {/each}
    </section>
  {/if}

  <section class="counts">
    <a class="card count" href="/stacks">
      <Icon name="layers" size={17} />
      <div><strong>{c?.stacks ?? '–'}</strong><span>Stacks</span></div>
      <small>{c ? `${c.stackStatus.running} running${c.stackStatus.partial ? ` · ${c.stackStatus.partial} partly` : ''}` : ''}{#if withUpdates.length}<span class="upd">{` · ${withUpdates.length} update${withUpdates.length === 1 ? '' : 's'}`}</span>{/if}</small>
    </a>
    <a class="card count" href="/containers">
      <Icon name="box" size={17} />
      <div><strong>{c?.containers ?? '–'}</strong><span>Containers</span></div>
      <small>{c ? `${c.containersRunning} running · ${c.containersStopped} stopped` : ''}</small>
    </a>
    {#if features.vms}
      <a class="card count" href="/vms">
        <Icon name="vm" size={17} />
        <div><strong>{vms ? vms.length : '–'}</strong><span>VMs</span></div>
        <small>{vms ? `${vms.filter((v) => v.state === 'running').length} running` : ''}</small>
      </a>
    {/if}
    <a class="card count" href="/images">
      <Icon name="disc" size={17} />
      <div><strong>{c?.images ?? '–'}</strong><span>Images</span></div>
      <small>{c ? bytes(c.imagesSize) : ''}</small>
    </a>
    <a class="card count" href="/volumes">
      <Icon name="database" size={17} />
      <div><strong>{c?.volumes ?? '–'}</strong><span>Volumes</span></div>
      <small>&nbsp;</small>
    </a>
    <a class="card count" href="/networks">
      <Icon name="network" size={17} />
      <div><strong>{c?.networks ?? '–'}</strong><span>Networks</span></div>
      <small>&nbsp;</small>
    </a>
  </section>

  <section class="card busiest">
    <div class="card-header"><h2>Busiest containers</h2></div>
    {#if !data}
      <div class="card-body"><div class="skeleton" style="height:120px"></div></div>
    {:else if !top.length}
      <div class="card-body dim small">{c?.containersRunning ? 'Measuring… figures appear in a few seconds.' : 'No containers are running.'}</div>
    {:else}
      <div class="trow thead">
        <span>Container</span>
        {#each [['cpu', 'CPU'], ['mem', 'Memory']] as [key, label]}
          <button class="sort" class:on={topBy === key} aria-pressed={topBy === key} title="Busiest by {label.toLowerCase()}" onclick={() => (topBy = key)}>
            {label}{#if topBy === key}<Icon name="chevron-down" size={13} />{/if}
          </button>
        {/each}
      </div>
      {#each top as t (t.id)}
        <a class="trow" href="/containers/{t.id}">
          <span class="tname"><strong class="truncate">{t.name}</strong>{#if t.stack}<span class="dim small truncate">{t.stack}</span>{/if}</span>
          <div class="metric"><div class="meter thin"><div style="width:{pct(t.cpu, cpuMax)}%"></div></div><span class="tval">{percent(t.cpu)}</span></div>
          <div class="metric"><div class="meter thin"><div style="width:{pct(t.mem, memMax)}%"></div></div><span class="tval">{bytes(t.mem)}</span></div>
        </a>
      {/each}
    {/if}
  </section>
</div>

<style>
  .section { font-size: 13px; font-weight: 600; color: var(--text-2); margin: 4px 0 10px; }
  .grid4 { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px; margin-bottom: 14px; }
  .tile { padding: 14px 16px 12px; display: grid; gap: 6px; align-content: start; }
  .label { display: flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); font-weight: 500; }
  .temp { margin-left: auto; display: inline-flex; align-items: center; gap: 2px; font-size: 12px; color: var(--text-2); font-variant-numeric: tabular-nums; }
  .temp.amber { color: var(--amber-text); }
  .temp.red { color: var(--red-text); }
  .value { font-size: 22px; font-weight: 600; letter-spacing: -0.02em; font-variant-numeric: tabular-nums; white-space: nowrap; }
  .value small { font-size: 13px; font-weight: 400; color: var(--text-3); }
  .value.two { display: flex; gap: 14px; font-size: 17px; flex-wrap: wrap; }
  .arrow { font-size: 12px; font-weight: 600; margin-right: 4px; }
  .arrow.in { color: var(--accent); }
  .arrow.out { color: var(--green); }
  .tile:nth-child(4) .arrow.out { color: var(--amber); }
  .foot { font-size: 12px; color: var(--text-3); min-height: 17px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .stackbar { display: flex; height: 7px; border-radius: 99px; overflow: hidden; background: var(--surface-2); }
  .seg.used { background: var(--accent); }
  .seg.arc { background: color-mix(in srgb, var(--accent) 45%, var(--surface-2)); }
  .seg.cache { background: color-mix(in srgb, var(--text-3) 35%, var(--surface-2)); }
  .storage { display: grid; grid-template-columns: repeat(auto-fit, minmax(165px, 1fr)); gap: 12px; margin-bottom: 18px; }
  .mini { padding: 12px 14px; display: grid; gap: 5px; align-content: start; color: var(--text); transition: border-color 0.12s; }
  a.mini:hover { text-decoration: none; border-color: var(--border-strong); }
  .mini .label { font-size: 12.5px; }
  .mv { font-size: 16px; font-weight: 600; font-variant-numeric: tabular-nums; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .mv small { font-size: 12px; font-weight: 400; color: var(--text-3); }
  .green-t { color: var(--green-text); }
  .amber-t { color: var(--amber-text); }
  .red-t { color: var(--red-text); }
  .jobline { display: flex; align-items: center; gap: 10px; padding: 10px 16px; margin-bottom: 12px; color: var(--text); font-size: 13.5px; border-color: color-mix(in srgb, var(--accent) 40%, var(--border)); }
  .jobline:hover { text-decoration: none; }
  .jobline :global(svg) { color: var(--accent); flex: none; }
  .grow { flex: 1; min-width: 0; }
  .meter.thin { height: 5px; }
  .note { display: flex; gap: 8px; align-items: center; padding: 10px 14px; border-radius: 10px; margin-bottom: 10px; font-size: 13.5px; }
  .note.err { background: var(--red-soft); color: var(--red-text); }
  .note:hover { text-decoration: none; filter: brightness(0.98); }
  .counts { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; margin-bottom: 18px; }
  .count { display: grid; grid-template-columns: auto 1fr; gap: 2px 11px; padding: 11px 14px; color: var(--text); align-items: center; transition: border-color 0.12s; }
  .count:hover { text-decoration: none; border-color: var(--border-strong); }
  .count > :global(svg) { color: var(--accent); grid-row: span 2; align-self: start; margin-top: 3px; }
  .count div { display: flex; align-items: baseline; gap: 7px; }
  .count strong { font-size: 18px; font-weight: 600; }
  .count span { color: var(--text-2); font-size: 13px; }
  .count small { grid-column: 2; color: var(--text-3); font-size: 12px; }
  .count .upd { color: var(--amber-text); }
  .sys-card, .busiest { margin-bottom: 18px; }
  .sys { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 230px), 1fr)); gap: 12px 24px; margin: 0; padding: 14px 18px; font-size: 13px; }
  .sys > div { display: grid; gap: 2px; min-width: 0; align-content: start; }
  .sys dt { color: var(--text-3); font-size: 12px; }
  .sys dd { margin: 0; overflow-wrap: anywhere; }
  .trow { display: grid; grid-template-columns: minmax(0, 1.3fr) minmax(0, 1fr) minmax(0, 1fr); gap: 24px; align-items: center; padding: 9px 18px; border-bottom: 1px solid var(--border); color: var(--text); font-size: 13.5px; }
  .trow:last-child { border-bottom: 0; }
  a.trow:hover { background: var(--surface-2); text-decoration: none; }
  .thead { padding-top: 7px; padding-bottom: 7px; font-size: 12px; color: var(--text-3); }
  .sort { all: unset; cursor: pointer; display: inline-flex; align-items: center; gap: 3px; color: var(--text-3); }
  .sort:hover, .sort.on { color: var(--text); }
  .sort:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 4px; }
  .tname { display: grid; min-width: 0; line-height: 1.3; }
  .metric { display: grid; grid-template-columns: minmax(40px, 1fr) 76px; gap: 12px; align-items: center; min-width: 0; }
  .tval { text-align: right; font-variant-numeric: tabular-nums; color: var(--text-2); }
  .small { font-size: 12px; }
  .error-card { display: flex; gap: 8px; align-items: center; padding: 12px 16px; color: var(--red-text); margin-bottom: 14px; }
  @media (max-width: 1100px) { .grid4 { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
  @media (max-width: 640px) {
    .trow { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 6px 16px; }
    .tname, .thead > span { grid-column: 1 / -1; }
    .thead > span { display: none; }
    .metric { grid-template-columns: minmax(30px, 1fr) auto; gap: 8px; }
    .sys { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px 16px; }
  }
  @media (max-width: 560px) { .grid4 { grid-template-columns: 1fr; } }
</style>
