<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';

  let st = $state(null);
  let error = $state('');
  let drafts = $state({}); // header id -> editable config
  let labels = $state({});
  let dirty = $state(false);
  let saving = $state(false);
  let showUnused = $state(false);
  let open = $state({});

  const presets = {
    disks: { label: 'Disk fans', sources: ['group/array'], curve: [{ temp: 35, percent: 30 }, { temp: 45, percent: 100 }], minPercent: 25 },
    cpu: { label: 'CPU fan', sources: [], curve: [{ temp: 40, percent: 30 }, { temp: 60, percent: 55 }, { temp: 80, percent: 100 }], minPercent: 25 },
  };

  async function load(first = false) {
    try {
      const s = await api.get('/host/status');
      st = s;
      if (first || !dirty) {
        const cfg = await api.get('/host/fans/config');
        labels = { ...(cfg.labels || {}) };
        const d = {};
        for (const f of s.fans) {
          const c = cfg.fans.find((x) => x.id === f.id) || { id: f.id, mode: 'bios' };
          d[f.id] = $state.snapshot({ name: '', percent: 50, sources: [], curve: [], minPercent: 0, ...c });
        }
        drafts = d;
      }
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(() => {
    load(true);
    const t = setInterval(() => load(false), 3000);
    return () => clearInterval(t);
  });

  const cpuSensor = $derived(st?.sensors.find((s) => s.kind === 'cpu'));
  const used = $derived((st?.fans || []).filter((f) => f.seenSpin || drafts[f.id]?.mode !== 'bios'));
  const unused = $derived((st?.fans || []).filter((f) => !f.seenSpin && drafts[f.id]?.mode === 'bios'));
  const sensorGroups = $derived.by(() => {
    const g = { group: 'Groups', cpu: 'CPU', board: 'Motherboard', nvme: 'NVMe', disk: 'Hard drives', ssd: 'SSDs', gpu: 'Graphics', other: 'Other' };
    const out = [];
    for (const [k, label] of Object.entries(g)) {
      const items = (st?.sensors || []).filter((s) => s.kind === k);
      if (items.length) out.push({ label, items });
    }
    return out;
  });

  function change() {
    dirty = true;
  }
  function setMode(id, mode) {
    const d = drafts[id];
    d.mode = mode;
    if (mode === 'curve' && d.curve.length < 2) applyPreset(id, cpuSensor && /cpu/i.test(d.name) ? 'cpu' : 'disks');
    if (mode === 'fixed' && !d.percent) d.percent = 50;
    change();
  }
  function applyPreset(id, key) {
    const p = presets[key];
    const d = drafts[id];
    d.curve = $state.snapshot(p.curve);
    d.sources = key === 'cpu' && cpuSensor ? [cpuSensor.id] : [...p.sources];
    d.minPercent = p.minPercent;
    if (!d.name) d.name = p.label;
    change();
  }
  function toggleSource(id, sid) {
    const d = drafts[id];
    d.sources = d.sources.includes(sid) ? d.sources.filter((x) => x !== sid) : [...d.sources, sid];
    change();
  }
  function addPoint(id) {
    const c = drafts[id].curve;
    const last = c[c.length - 1] || { temp: 30, percent: 30 };
    c.push({ temp: Math.min(110, last.temp + 5), percent: Math.min(100, last.percent + 10) });
    change();
  }
  function removePoint(id, i) {
    drafts[id].curve.splice(i, 1);
    change();
  }

  async function save() {
    saving = true;
    try {
      const fans = Object.values(drafts)
        .filter((d) => d.mode !== 'bios' || d.name)
        .map((d) => ({
          id: d.id, name: d.name || undefined, mode: d.mode,
          percent: d.mode === 'fixed' ? +d.percent : undefined,
          sources: d.mode === 'curve' ? d.sources : undefined,
          curve: d.mode === 'curve' ? d.curve.map((p) => ({ temp: +p.temp, percent: +p.percent })).sort((a, b) => a.temp - b.temp) : undefined,
          minPercent: d.mode !== 'bios' ? +d.minPercent || 0 : undefined,
        }));
      await api.put('/host/fans/config', { fans, labels });
      dirty = false;
      toast('Fan settings saved and applied', 'success', 2500);
      await load(true);
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    saving = false;
  }
  async function identify(id) {
    try {
      await api.post('/host/fans/test', { id, percent: 100, seconds: 10 });
      toast('Running at 100% for 10 seconds. Listen for which fan speeds up.', 'info', 6000);
    } catch (e) {
      toast(e.message, 'error');
    }
  }

  // Curve chart: 20–90°C across, 0–100% up.
  const W = 300, H = 140, T0 = 20, T1 = 90;
  const x = (t) => ((Math.max(T0, Math.min(T1, t)) - T0) / (T1 - T0)) * W;
  const y = (p) => H - (p / 100) * H;
  function path(curve) {
    if (!curve?.length) return '';
    const pts = [...curve].sort((a, b) => a.temp - b.temp);
    let d = `M0,${y(pts[0].percent)} L${x(pts[0].temp)},${y(pts[0].percent)}`;
    for (const p of pts.slice(1)) d += ` L${x(p.temp)},${y(p.percent)}`;
    return d + ` L${W},${y(pts[pts.length - 1].percent)}`;
  }
  const title = (f) => drafts[f.id]?.name || `Header ${f.index}`;
  const modeLabel = { bios: 'BIOS', fixed: 'Fixed', curve: 'Curve' };
  const reasonText = { failsafe: 'Full speed: a temperature it needs can\'t be read', asleep: 'Disks asleep: running at the bottom of the curve', test: 'Test: 100% for a few seconds' };
</script>

<div class="page">
  <div class="page-header">
    <div><h1>Fans</h1><p class="sub">Each fan can follow the hottest of any temperatures you pick. Sleeping disks are never woken to check them.</p></div>
    <button class="primary" onclick={save} disabled={!dirty || saving}><Icon name="save" size={15} /> {dirty ? 'Save and apply' : 'Saved'}</button>
  </div>

  {#if error}
    <div class="card note err"><Icon name="alert" size={16} /> {error}</div>
  {:else if !st}
    <div class="skeleton" style="height:360px"></div>
  {:else}
    <section class="card temps">
      <div class="card-header"><h2>Temperatures</h2></div>
      <div class="card-body tgroups">
        {#each sensorGroups as g}
          <div class="tgroup">
            <h3>{g.label}</h3>
            {#each g.items as s (s.id)}
              <div class="trow"><span class="truncate">{s.label}</span><strong class={s.temp >= 55 ? 'red-t' : s.temp >= 45 ? 'amber-t' : ''}>{s.temp != null ? `${Math.round(s.temp)}°C` : s.asleep ? 'asleep' : '–'}</strong></div>
            {/each}
          </div>
        {/each}
      </div>
    </section>

    {#if !st.fans.length}
      <div class="card body-pad dim">No controllable fan headers were found. The motherboard's sensor chip may need a driver (run <code>sudo sensors-detect</code>).</div>
    {/if}

    <div class="fans">
      {#each [...used, ...(showUnused ? unused : [])] as f (f.id)}
        {@const d = drafts[f.id]}
        {#if d}
          <section class="card fan" class:stalled={f.stalled}>
            <div class="fhead">
              <Icon name="fan" size={20} class={f.rpm > 0 ? 'spinning' : ''} />
              <div class="grow">
                <strong>{title(f)}</strong>
                <div class="dim small">{f.chip} · pwm{f.index}</div>
              </div>
              <div class="rpm"><strong>{f.rpm ?? '–'}</strong><span>RPM</span></div>
              <div class="rpm"><strong>{f.percent}%</strong><span>{modeLabel[f.mode]}</span></div>
            </div>
            {#if f.stalled}<div class="note error"><Icon name="alert" size={14} /> Set to {f.percent}% but not spinning. Check the fan and its cable.</div>{/if}
            {#if f.reason}<div class="note {f.reason === 'failsafe' ? 'error' : 'info'}"><Icon name="info" size={14} /> {reasonText[f.reason]}</div>{/if}
            {#if f.mode === 'curve' && f.sourceTemp != null}<div class="dim small">Following {f.source}: {Math.round(f.sourceTemp)}°C</div>{/if}

            <div class="segmented">
              {#each ['bios', 'fixed', 'curve'] as m}
                <button type="button" class:active={d.mode === m} onclick={() => setMode(f.id, m)}>{m === 'bios' ? 'BIOS controls it' : m === 'fixed' ? 'Fixed speed' : 'Temperature curve'}</button>
              {/each}
            </div>

            {#if d.mode === 'fixed'}
              <label class="field"><span>Speed: {d.percent}%</span><input type="range" min="20" max="100" bind:value={d.percent} oninput={change} /></label>
            {:else if d.mode === 'curve'}
              <div class="curvebox">
                <svg viewBox="-36 -8 {W + 44} {H + 30}" class="chart" role="img" aria-label="Fan curve">
                  {#each [0, 25, 50, 75, 100] as p}<line x1="0" x2={W} y1={y(p)} y2={y(p)} class="grid" /><text x="-6" y={y(p) + 3} class="ax" text-anchor="end">{p}%</text>{/each}
                  {#each [20, 30, 40, 50, 60, 70, 80, 90] as t}<text x={x(t)} y={H + 16} class="ax" text-anchor="middle">{t}°</text>{/each}
                  <path d={path(d.curve)} class="curve" />
                  {#each d.curve as p}<circle cx={x(p.temp)} cy={y(p.percent)} r="4" class="pt" />{/each}
                  {#if f.sourceTemp != null && f.mode === 'curve'}<line x1={x(f.sourceTemp)} x2={x(f.sourceTemp)} y1="0" y2={H} class="now" /><circle cx={x(f.sourceTemp)} cy={y(f.percent)} r="5" class="nowpt" />{/if}
                </svg>
                <div class="points">
                  {#each d.curve as p, i}
                    <div class="point"><input type="number" min="0" max="120" bind:value={p.temp} oninput={change} aria-label="Temperature" /><span>°C →</span><input type="number" min="0" max="100" bind:value={p.percent} oninput={change} aria-label="Speed" /><span>%</span>
                      <button class="ghost icon xs" aria-label="Remove point" disabled={d.curve.length <= 2} onclick={() => removePoint(f.id, i)}><Icon name="x" size={12} /></button></div>
                  {/each}
                  <div class="row wrap"><button class="sm ghost" onclick={() => addPoint(f.id)} disabled={d.curve.length >= 10}><Icon name="plus" size={12} /> Point</button>
                    <button class="sm ghost" onclick={() => applyPreset(f.id, 'disks')}>Disk preset</button>
                    <button class="sm ghost" onclick={() => applyPreset(f.id, 'cpu')}>CPU preset</button></div>
                </div>
              </div>
              <div class="field"><span>Follow the hottest of</span>
                <div class="sources">
                  {#each sensorGroups as g}
                    {#each g.items as s (s.id)}
                      <label class="src" class:on={d.sources.includes(s.id)}><input type="checkbox" checked={d.sources.includes(s.id)} onchange={() => toggleSource(f.id, s.id)} /> {s.label}<span class="dim">{s.temp != null ? ` ${Math.round(s.temp)}°` : s.asleep ? ' asleep' : ''}</span></label>
                    {/each}
                  {/each}
                </div>
              </div>
            {/if}

            <div class="row wrap foot">
              <label class="inline">Name <input bind:value={d.name} oninput={change} placeholder="Header {f.index}" maxlength="40" /></label>
              {#if d.mode !== 'bios'}<label class="inline">Never below <input type="number" min="0" max="100" bind:value={d.minPercent} oninput={change} class="num" />%</label>{/if}
              <span class="spacer"></span>
              <button class="sm" onclick={() => identify(f.id)}><Icon name="activity" size={13} /> Identify</button>
            </div>
          </section>
        {/if}
      {/each}
    </div>
    {#if unused.length}
      <button class="ghost sm more" onclick={() => (showUnused = !showUnused)}>{showUnused ? 'Hide' : 'Show'} {unused.length} header{unused.length === 1 ? '' : 's'} with no fan detected</button>
    {/if}
    <p class="hint">If the fan service stops for any reason, every header goes back to the BIOS's own control. A fan that needs a temperature which can't be read runs at full speed until it can.</p>
  {/if}
</div>

<style>
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .grow { flex: 1; min-width: 0; }
  .spacer { flex: 1; }
  .small { font-size: 12.5px; }
  .note { display: flex; gap: 8px; align-items: center; padding: 8px 12px; border-radius: 8px; font-size: 13px; }
  .note.error, .note.err { background: var(--red-soft); color: var(--red-text); }
  .note.info { background: var(--accent-soft); color: var(--accent-text); }
  .temps { margin-bottom: 16px; }
  .tgroups { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 16px; }
  .tgroup h3 { margin: 0 0 6px; font-size: 12px; text-transform: uppercase; letter-spacing: 0.04em; color: var(--text-3); }
  .trow { display: flex; justify-content: space-between; gap: 8px; font-size: 13px; padding: 2px 0; }
  .fans { display: grid; grid-template-columns: repeat(auto-fill, minmax(420px, 1fr)); gap: 14px; }
  .fan { padding: 16px; display: grid; gap: 12px; align-content: start; }
  .fan.stalled { border-color: var(--red); }
  .fhead { display: flex; align-items: center; gap: 12px; }
  .fhead :global(svg) { color: var(--text-3); }
  .fhead :global(.spinning) { color: var(--accent); animation: spin 1.4s linear infinite; }
  .rpm { display: grid; justify-items: end; min-width: 58px; }
  .rpm strong { font-size: 18px; font-variant-numeric: tabular-nums; }
  .rpm span { font-size: 11.5px; color: var(--text-3); }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  input[type='range'] { height: auto; padding: 0; border: 0; accent-color: var(--accent); }
  .curvebox { display: grid; grid-template-columns: 1fr; gap: 10px; align-items: start; }
  .chart { width: 100%; height: auto; max-height: 220px; }
  .chart .grid { stroke: var(--border); stroke-width: 1; }
  .chart .ax { fill: var(--text-3); font-size: 9px; }
  .chart .curve { fill: none; stroke: var(--accent); stroke-width: 2.5; stroke-linejoin: round; }
  .chart .pt { fill: var(--surface); stroke: var(--accent); stroke-width: 2; }
  .chart .now { stroke: var(--amber); stroke-dasharray: 3 3; }
  .chart .nowpt { fill: var(--amber); }
  .points { display: flex; flex-wrap: wrap; gap: 6px 14px; align-items: center; }
  .point { display: flex; align-items: center; gap: 4px; font-size: 12.5px; color: var(--text-2); white-space: nowrap; }
  .points .row button { white-space: nowrap; }
  .point input { width: 58px; height: 28px; padding: 0 6px; }
  .sources { display: flex; flex-wrap: wrap; gap: 6px; max-height: 190px; overflow: auto; padding-bottom: 2px; }
  .src { display: inline-flex; align-items: center; gap: 5px; padding: 3px 9px; border: 1px solid var(--border); border-radius: 99px; font-size: 12.5px; cursor: pointer; }
  .src.on { border-color: var(--accent); background: var(--accent-soft); color: var(--accent-text); }
  .src input { width: 13px; height: 13px; margin: 0; accent-color: var(--accent); }
  .foot { border-top: 1px solid var(--border); padding-top: 10px; }
  .inline { display: inline-flex; align-items: center; gap: 6px; font-size: 13px; color: var(--text-2); }
  .inline input { height: 30px; width: 150px; }
  .inline input.num { width: 60px; }
  .more { margin-top: 12px; }
  .red-t { color: var(--red-text); }
  .amber-t { color: var(--amber-text); }
  @keyframes spin { to { transform: rotate(360deg); } }
  @media (max-width: 520px) { .fans { grid-template-columns: 1fr; } .curvebox { grid-template-columns: 1fr; } }
</style>
