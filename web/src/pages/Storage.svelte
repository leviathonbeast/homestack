<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast, confirm, session } from '../lib/state.svelte.js';
  import { bytes, ago, dateTime, duration } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from '../components/Modal.svelte';
  import Menu from '../components/Menu.svelte';
  import Pager, { pageOf } from '../components/Pager.svelte';
  import ArrayDevices from '../components/ArrayDevices.svelte';

  let st = $state(null);
  let error = $state('');
  let busy = $state('');
  let logJob = $state(null);
  let logText = $state('');

  async function load() {
    try {
      st = await api.get('/host/status');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(() => {
    load();
    const t = setInterval(load, 4000);
    return () => clearInterval(t);
  });

  async function act(body, label) {
    busy = label || body.action;
    try {
      await api.post('/host/actions', body);
      await load();
    } catch (e) {
      toast(e.message, 'error', 9000);
    }
    busy = '';
  }

  const diskFor = (role) => st?.disks.find((d) => d.role === role);
  const jobVerb = (r) => (r.method?.startsWith('add-') ? 'Adding' : 'Replacing');
  const pct = (a, b) => (b ? Math.min(100, (a / b) * 100) : 0);
  const level = (p) => (p >= 90 ? 'red' : p >= 80 ? 'amber' : '');
  const arrayPools = $derived((st?.pools || []).filter((p) => p.array));
  const flattenLeft = $derived(arrayPools.reduce((n, p) => n + (p.datasets || 0), 0));
  const running = $derived(st?.running);
  const brokenSlots = $derived((st?.slots || []).filter((s) => s.state !== 'ONLINE'));
  // The last sync didn't complete parity (failed, refused, or stopped at a
  // safety limit: many files deleted or changed): say why. A skipped one
  // can be run anyway.
  const lastProblem = $derived.by(() => {
    const r = st?.parity?.lastRun;
    if (!r || r.ok || !r.summary) return '';
    if (st.parity.lastSync && new Date(r.finished) <= new Date(st.parity.lastSync)) return '';
    return r.summary.replace(/ Sync anyway from Storage if that was intended\.$/, '');
  });
  const skipped = $derived(lastProblem.startsWith('Skipped:'));
  const arrayState = $derived(st?.array?.state);

  async function stopArray() {
    const r = await confirm({
      title: 'Stop the array?',
      message: 'Stops every container except Homestack, shuts down the VMs and Samba, then unmounts the data disks. The cache and Homestack keep running, so you can start the array again from here.',
      confirmLabel: 'Stop array', danger: true, typeToConfirm: 'STOP',
      options: [{ key: 'force', label: 'Force VMs off if they don\'t shut down within 2½ minutes' }],
    });
    if (r) act({ action: 'array-stop', confirm: 'STOP', force: !!r.force }, 'array-stop');
  }
  async function power(action) {
    const word = action === 'reboot' ? 'REBOOT' : 'SHUTDOWN';
    const ok = await confirm({
      title: action === 'reboot' ? 'Reboot the server?' : 'Shut the server down?',
      message: `Containers, VMs and Samba are stopped cleanly first.${action === 'shutdown' ? ' You\'ll need the power button or IPMI to turn it back on.' : ' Homestack will be back in a few minutes.'}`,
      confirmLabel: action === 'reboot' ? 'Reboot' : 'Shut down', danger: true, typeToConfirm: word,
    });
    if (ok) act({ action, confirm: word }, action);
  }
  async function sync(force = false) {
    if (force) {
      const ok = await confirm({ title: 'Sync anyway?', message: `${lastProblem}\n\nOnly do this if you meant to delete or change them. Once synced, parity can no longer bring back the old files.`, confirmLabel: 'Sync anyway', danger: true, typeToConfirm: 'SYNC' });
      if (!ok) return;
    }
    act({ action: 'parity-sync', force }, 'parity-sync');
  }
  async function stopJob() {
    const ok = await confirm({ title: `Stop ${running.title.toLowerCase()}?`, message: 'It stops cleanly and can carry on from where it left off next time.', confirmLabel: 'Stop' });
    if (ok) act({ action: 'job-stop' }, 'job-stop');
  }
  // A parity job can be paused where it is, and carries on from there.
  const pausable = (j) => j?.action === 'parity-sync' || j?.action === 'parity-check';
  const pauseFor = (minutes) => act({ action: 'job-pause', minutes }, 'job-pause');
  const pauseItems = [
    ...[1, 2, 4, 8].map((h) => ({ label: `For ${h} hour${h === 1 ? '' : 's'}`, onclick: () => pauseFor(h * 60) })),
    { divider: true },
    { label: 'Until I resume it', hint: 'Until then the mover and scheduled parity jobs don\'t run', onclick: () => pauseFor(0) },
  ];
  const lastTrim = $derived(st?.jobs.find((j) => j.action === 'trim'));
  function etaText(sec) {
    if (sec < 60) return 'under a minute';
    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60);
    return h ? `${h}h ${String(m).padStart(2, '0')}m` : `${m} min`;
  }
  // A time of day, with the day when it isn't today.
  function at(when) {
    const t = new Date(when);
    const same = t.toDateString() === new Date().toDateString();
    const time = t.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    return same ? time : `${t.toLocaleDateString([], { weekday: 'short' })} ${time}`;
  }
  const finishAt = (sec) => at(Date.now() + sec * 1000);
  async function showLog(j) {
    logJob = j;
    logText = 'Loading…';
    try {
      const r = await fetch(`/api/host/jobs/${j.id}/log`, { credentials: 'same-origin' });
      logText = r.ok ? await r.text() : 'No log.';
    } catch {
      logText = 'Couldn\'t load the log.';
    }
  }
  const took = (j) => (j.finished ? duration((new Date(j.finished) - new Date(j.started)) / 1000) : '');
  const stateIcon = { ok: 'check', failed: 'alert', stopped: 'stop', running: 'loader' };

  // ---- History: jobs started here, and every run of the mover and the parity jobs, cron included ----
  const runKinds = [['jobs', 'Jobs'], ['parity-sync', 'Parity sync'], ['parity-check', 'Parity check'], ['mover', 'Mover']];
  let runKind = $state('jobs');
  let runs = $state(null);
  let runsError = $state('');
  let runsPage = $state(0);
  async function loadRuns() {
    try {
      runs = await api.get(`/host/runs?action=${runKind}`);
      runsError = '';
    } catch (e) {
      runsError = e.message;
    }
  }
  // Reload when the tab changes, and when a job starts or finishes.
  const jobKey = $derived(`${st?.running?.id || ''}|${st?.jobs?.[0]?.id || ''}|${st?.mover?.finished || ''}`);
  $effect(() => {
    jobKey;
    if (st && runKind !== 'jobs') loadRuns();
  });
  // Parity details: the settings the jobs use (Settings → Storage).
  let parityInfo = $state(null);
  async function showParity() {
    parityInfo = {};
    parityInfo = await api.get('/host/parity').catch(() => ({}));
  }

  async function showRun(r) {
    logJob = { title: `${runKinds.find((k) => k[0] === r.action)?.[1]} · ${dateTime(r.started)}` };
    logText = 'Loading…';
    try {
      const res = await fetch(`/api/host/runs/log?name=${encodeURIComponent(r.name)}`, { credentials: 'same-origin' });
      logText = res.ok ? await res.text() : (await res.json().catch(() => ({}))).error || 'No log.';
    } catch {
      logText = 'Couldn\'t load the log.';
    }
  }
  const runState = (r) => (r.running ? 'running' : r.ok == null ? 'stopped' : r.ok ? 'ok' : 'failed');
  const runTook = (r) => (r.running ? '' : duration((new Date(r.updated) - new Date(r.started)) / 1000));
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Storage</h1>
      <p class="sub">{st ? `${st.host} · array ${arrayState} · agent v${st.version}` : ' '}{st?.array?.busy ? ` · busy: ${st.array.busy}` : ''}</p>
    </div>
    {#if st}
      <div class="row wrap">
        {#if arrayState === 'started'}
          <button onclick={stopArray} disabled={!!busy || !!running}><Icon name="stop" size={15} /> Stop array</button>
        {:else}
          <button class="primary" onclick={() => act({ action: 'array-start' }, 'array-start')} disabled={!!busy || !!running}><Icon name="play" size={15} /> Start array</button>
        {/if}
        <button onclick={() => power('reboot')} disabled={!!busy || !!running}><Icon name="restart" size={15} /> Reboot</button>
        <button class="danger" onclick={() => power('shutdown')} disabled={!!busy || !!running}><Icon name="power" size={15} /> Shut down</button>
      </div>
    {/if}
  </div>

  {#if error}
    <div class="card note err"><Icon name="alert" size={16} /> {error}</div>
  {:else if !st}
    <div class="skeleton" style="height:420px"></div>
  {:else}
    {#if session.version && st.version && st.version !== session.version}
      <div class="note warning"><Icon name="info" size={16} /> The host agent is v{st.version} but Homestack is v{session.version}. Update it: <code>sudo bash {st.source ? st.source + '/' : ''}deploy/debian/agent/install.sh</code>{st.source ? '' : " (from Homestack's source folder)"}</div>
    {/if}
    {#each st.warnings as w}
      <div class="note {w.level}"><Icon name="alert" size={16} /> {w.text}</div>
    {/each}
    {#if st.replace}
      <a class="note {st.replace.state === 'running' ? 'info' : st.replace.state === 'done' ? 'success' : 'error'} replink" href="/storage/replace">
        <Icon name={st.replace.state === 'running' ? 'loader' : 'alert'} size={16} class={st.replace.state === 'running' ? 'spin' : ''} />
        <span><strong>{jobVerb(st.replace)} {st.replace.pool}</strong> · {st.replace.state === 'running' ? st.replace.step : st.replace.state === 'attention' ? 'needs your decision' : st.replace.state === 'paused' ? 'paused' : st.replace.state}. Open</span>
      </a>
    {/if}
    {#if flattenLeft > 0}
      <div class="note warning"><Icon name="info" size={16} /> {flattenLeft} share{flattenLeft === 1 ? ' is' : 's are'} still a separate ZFS dataset, which SnapRAID can't see. Run <code>flatten-dataset</code> on {flattenLeft === 1 ? 'it' : 'them'} before setting up parity.</div>
    {/if}

    {#if running}
      <section class="card runjob">
        <div class="card-header">
          {#if running.paused}<Icon name="pause" size={16} />{:else}<Icon name="loader" size={16} class="spin" />{/if}<h2>{running.title}</h2>
          {#if running.paused}<span class="badge amber">Paused</span>{/if}
          <span class="dim small hide-sm">started {ago(running.started)}</span>
          <span class="spacer"></span>
          <button class="sm ghost" onclick={() => showLog(running)}>Log</button>
          {#if pausable(running)}
            {#if running.paused}
              <button class="sm primary" onclick={() => act({ action: 'job-resume' }, 'job-resume')} disabled={!!busy}><Icon name="play" size={13} /> Resume</button>
            {:else}
              <Menu text="Pause" icon="pause" size="sm" items={pauseItems} />
            {/if}
          {/if}
          {#if !['array-stop', 'array-start', 'reboot', 'shutdown'].includes(running.action)}
            <button class="sm" onclick={stopJob} disabled={busy === 'job-stop'}>Stop</button>
          {/if}
        </div>
        <div class="card-body stack-v">
          {#if running.progress != null}
            <div class="meter big" class:amber={running.paused}><div style="width:{running.progress}%"></div></div>
            <div class="progline">
              <strong>{running.progress}%</strong>
              {#if running.doneBytes != null}<span>{running.totalBytes ? `${bytes(running.doneBytes)} of ${bytes(running.totalBytes)} done` : `${bytes(running.doneBytes)} read`}</span>{/if}
              {#if running.speed}<span class="dim">{running.speed}</span>{/if}
              {#if running.etaSeconds != null}<span><strong>{etaText(running.etaSeconds)}</strong> to go</span><span class="dim">finishes around {finishAt(running.etaSeconds)}</span>{:else if running.eta}<span>{running.eta} left</span>{/if}
            </div>
          {:else if running.step}
            <div class="small">{running.step}</div>
          {/if}
          {#if running.paused}
            <div class="small amber-t">Paused at {at(running.paused)}, {running.resumeAt ? `carries on by itself at ${at(running.resumeAt)}` : 'until you resume it'}. It keeps its place; the mover and scheduled parity jobs don't run meanwhile.</div>
          {/if}
        </div>
      </section>
    {/if}

    <section class="summary">
      <div class="card tile">
        <div class="label"><Icon name="database" size={15} /> Array</div>
        <div class="value">{bytes(st.totals.used, 1)} <small>of {bytes(st.totals.size, 1)}</small></div>
        <div class="meter {level(pct(st.totals.used, st.totals.size))}"><div style="width:{pct(st.totals.used, st.totals.size)}%"></div></div>
        <div class="foot">{bytes(st.totals.free, 1)} free across {st.totals.disks} disks</div>
      </div>
      {#if st.cache}
        <div class="card tile">
          <div class="label"><Icon name="disk" size={15} /> Cache</div>
          <div class="value">{bytes(st.cache.used, 0)} <small>of {bytes(st.cache.size, 0)}</small></div>
          <div class="meter {level(pct(st.cache.used, st.cache.size))}"><div style="width:{pct(st.cache.used, st.cache.size)}%"></div></div>
          <div class="foot">{bytes(st.cache.avail, 0)} free · {st.cache.fstype}{diskFor('cache')?.temp ? ` · ${diskFor('cache').temp}°C` : ''}</div>
          {#if lastTrim?.finished}<div class="foot" title={lastTrim.detail}>TRIM {ago(lastTrim.finished)}{lastTrim.state === 'ok' ? '' : ` · ${lastTrim.state}`}</div>{/if}
          <div class="row btns"><button class="sm" onclick={() => act({ action: 'trim' })} disabled={!!busy || !!running} title="Tells every SSD with a mounted filesystem, and each ZFS pool on SSDs, which blocks are free, so they stay fast. The log is in History."><Icon name="scissors" size={13} /> TRIM now</button></div>
        </div>
      {/if}
      <div class="card tile parity">
        <div class="label"><Icon name="shield" size={15} /> Parity</div>
        {#if !st.parity.installed}
          <div class="value small-v">Not installed</div><div class="foot">SnapRAID isn't installed on this server.</div>
        {:else if !st.parity.configured}
          <div class="value small-v">Not set up yet</div><div class="foot">Create /etc/snapraid.conf to protect the array.</div>
        {:else if st.replace}
          <div class="value small-v amber-t">On hold</div>
          <div class="foot">{jobVerb(st.replace)} {st.replace.pool}: parity is updated at the end. <a href="/storage/replace">See progress</a>.</div>
        {:else if brokenSlots.length}
          <div class="value small-v red-t">Degraded</div>
          <div class="foot">{brokenSlots.map((s) => s.pool).join(', ')} {brokenSlots.length === 1 ? 'is' : 'are'} missing. Parity still covers {brokenSlots.length === 1 ? 'its' : 'their'} files as of {st.parity.lastSync ? ago(st.parity.lastSync) : 'the last sync'}; <a href="/storage/replace">replace</a> to rebuild.</div>
        {:else if st.parity.syncing}
          <div class="value small-v amber-t">{running?.action === 'parity-sync' && running.paused ? 'Sync paused' : 'Syncing'}{running?.action === 'parity-sync' && running.progress != null ? ` · ${running.progress}%` : '…'}</div>
          <div class="foot">{st.parity.synced ? `Last complete sync ${ago(st.parity.lastSync)}. Changes since then are protected when this finishes.` : 'First sync: nothing is protected until it finishes.'}</div>
        {:else if !st.parity.synced}
          <div class="value small-v">No parity yet</div><div class="foot">Run the first sync. Until then nothing is protected.</div>
        {:else}
          <div class="value small-v {st.parity.daysSinceSync >= 3 || lastProblem ? 'amber-t' : 'green-t'}">{st.parity.daysSinceSync === 0 ? 'Up to date' : `${st.parity.daysSinceSync} day${st.parity.daysSinceSync === 1 ? '' : 's'} behind`}</div>
          <div class="foot">Synced {ago(st.parity.lastSync)}{st.parity.lastCheck ? ` · checked ${ago(st.parity.lastCheck.finished)}` : ''}</div>
        {/if}
        {#if st.parity.configured}
          {#if lastProblem && !st.parity.syncing}<div class="foot amber-t" title={lastProblem}>Last sync: {lastProblem}</div>{/if}
          <div class="row btns">
            <button class="sm primary" onclick={() => sync(false)} disabled={!!busy || !!running || brokenSlots.length > 0 || !!st.hold}><Icon name="refresh" size={13} /> Sync now</button>
            {#if skipped && !st.parity.syncing}<button class="sm danger" onclick={() => sync(true)} disabled={!!busy || !!running || brokenSlots.length > 0 || !!st.hold}>Sync anyway</button>{/if}
            <button class="sm" onclick={() => act({ action: 'parity-check' })} disabled={!!busy || !!running || !st.parity.synced || brokenSlots.length > 0 || !!st.hold}><Icon name="check" size={13} /> Check</button>
            <button class="sm ghost" onclick={showParity}>Details</button>
          </div>
        {/if}
      </div>
      <div class="card tile">
        <div class="label"><Icon name="upgrade" size={15} /> Mover</div>
        {#if st.mover}
          <div class="value small-v {st.mover.ok ? '' : 'red-t'}">{st.mover.ok ? 'Last run OK' : 'Last run failed'}</div>
          <div class="foot" title={st.mover.summary}>{ago(st.mover.finished)} · {st.mover.summary}</div>
        {:else}
          <div class="value small-v">Not run yet</div><div class="foot">Set when it runs on <a href="/schedules">Schedules</a>.</div>
        {/if}
        <div class="row btns"><button class="sm" onclick={() => act({ action: 'mover' })} disabled={!!busy || !!running || arrayState !== 'started' || !!st.hold}><Icon name="play" size={13} /> Run now</button></div>
      </div>
      {#if st.ups}
        {@const u = st.ups}
        <div class="card tile">
          <div class="label"><Icon name="power" size={15} /> UPS</div>
          {#if u.error}
            <div class="value small-v amber-t">No contact</div><div class="foot">{u.error}</div>
          {:else}
            <div class="value small-v {u.onBattery ? 'red-t' : 'green-t'}">{u.onBattery ? 'On battery' : 'On mains'}{#if u.lowBattery} · low{/if}</div>
            {#if u.charge != null}<div class="meter {u.charge < 40 ? 'red' : u.charge < 70 ? 'amber' : ''}"><div style="width:{u.charge}%"></div></div>{/if}
            <div class="foot">{u.charge != null ? `${Math.round(u.charge)}%` : ''}{u.runtime != null ? ` · ${Math.round(u.runtime / 60)} min runtime` : ''}{u.load != null ? ` · ${Math.round(u.load)}% load` : ''}{u.power != null ? ` (${Math.round(u.power)} W)` : ''}</div>
          {/if}
        </div>
      {/if}
    </section>

    <div class="devices"><ArrayDevices disks={st.disks} pools={st.pools} cacheFS={st.cache} act={(b) => act(b)} busy={!!busy || !!running} refreshKey={`${st.replace?.state}|${st.replace?.step}|${st.running?.id}`} /></div>

    <section class="card">
      <div class="card-header wrap">
        <h2>History</h2>
        <span class="dim small hide-sm">{runKind === 'jobs' ? 'Jobs started here' : 'Every run, scheduled or started here'}</span>
        <span class="spacer"></span>
        <div class="segmented">
          {#each runKinds as [k, l]}<button class:active={runKind === k} onclick={() => { runKind = k; runsPage = 0; }}>{l}</button>{/each}
        </div>
      </div>
      {#if runKind === 'jobs'}
        {#if st.jobs.length}
          <div class="jobs">
            {#each pageOf(st.jobs, runsPage, 5) as j (j.id)}
              <div class="job {j.state}">
                <Icon name={stateIcon[j.state] || 'info'} size={15} class={j.state === 'running' ? 'spin' : ''} />
                <div class="grow"><strong>{j.title}</strong><div class="dim small truncate" title={j.detail}>{j.detail}</div></div>
                <span class="dim small nowrap hide-sm">{dateTime(j.started)}{took(j) ? ` · ${took(j)}` : ''}</span>
                <button class="sm ghost" onclick={() => showLog(j)}>Log</button>
              </div>
            {/each}
          </div>
          <Pager total={st.jobs.length} size={5} bind:page={runsPage} />
        {:else}
          <div class="card-body dim small">Nothing yet. Jobs started here, and their results, appear in this list.</div>
        {/if}
      {:else if runsError}
        <div class="card-body dim small">{runsError}</div>
      {:else if !runs}
        <div class="card-body"><div class="skeleton" style="height:80px"></div></div>
      {:else if runs.length}
        <div class="jobs">
          {#each pageOf(runs, runsPage, 5) as r (r.name)}
            {@const s = runState(r)}
            <div class="job {s}">
              <Icon name={stateIcon[s]} size={15} class={s === 'running' ? 'spin' : ''} />
              <div class="grow">
                <strong>{dateTime(r.started)}</strong>{#if r.jobId}<span class="badge tiny">from Homestack</span>{/if}
                <div class="dim small truncate" title={r.summary}>{r.running ? 'Running now' : r.summary || (r.ok == null ? 'Didn\'t finish (stopped, or the server went down)' : '')}</div>
              </div>
              <span class="dim small nowrap hide-sm">{runTook(r)}{r.size ? ` · ${bytes(r.size)}${r.name.endsWith('.gz') ? ' packed' : ''}` : ''}</span>
              <button class="sm ghost" onclick={() => showRun(r)}>View</button>
            </div>
          {/each}
        </div>
        <Pager total={runs.length} size={5} bind:page={runsPage} />
      {:else}
        <div class="card-body dim small">No runs logged yet. Each run keeps its own log here once the updated scripts are installed (<code>deploy/debian/agent/install.sh</code>).</div>
      {/if}
    </section>
  {/if}
</div>

{#if parityInfo && st}
  {@const pv = parityInfo.values}
  <Modal title="Parity" width={640} onclose={() => (parityInfo = null)}>
    <div class="stack-v">
      <dl class="kv">
        <dt>Last complete sync</dt><dd>{st.parity.lastSync ? `${dateTime(st.parity.lastSync)} (${ago(st.parity.lastSync)})` : 'Never'}</dd>
        <dt>Last check</dt><dd>{st.parity.lastCheck ? `${dateTime(st.parity.lastCheck.finished)} · ${st.parity.lastCheck.ok ? 'OK' : 'failed'}` : 'Never'}</dd>
        {#if pv}
          <dt>Each check</dt><dd>Reads {pv.SNAPRAID_SCRUB_PERCENT}% of the array, from blocks {pv.SNAPRAID_SCRUB_AGE ? `not checked for ${pv.SNAPRAID_SCRUB_AGE} day${pv.SNAPRAID_SCRUB_AGE === 1 ? '' : 's'}` : 'of any age'}, oldest first</dd>
          <dt>A sync waits</dt><dd>When more than {pv.SNAPRAID_DELETE_LIMIT} files were deleted, or {pv.SNAPRAID_UPDATE_LIMIT} modified, since the last one</dd>
        {/if}
        {#if st.parity.version}<dt>SnapRAID</dt><dd>{st.parity.version.replace(/ by .*$/, '')}</dd>{/if}
      </dl>
      <p class="small"><a href="/settings/storage">Change these in Settings → Storage</a>. When the sync and check run is on <a href="/schedules">Schedules</a>.</p>
      {#if st.parity.summary.length}
        <div>
          <div class="what">Status{st.parity.checked ? `, as of ${ago(st.parity.checked)}` : ''}</div>
          <ul class="plain">{#each st.parity.summary as line}<li>{line}</li>{/each}</ul>
        </div>
      {/if}
      <div class="row wrap small dim">{#each st.parity.data as d}<span class="chip"><span class="dot {d.mounted ? 'green' : 'red'}"></span>{d.name}</span>{/each}{#each st.parity.parity as d}<span class="chip"><span class="dot {d.mounted ? 'green' : 'red'}"></span>{d.name}</span>{/each}</div>
    </div>
  </Modal>
{/if}

{#if logJob}
  <Modal title="{logJob.title} log" width={820} onclose={() => (logJob = null)}>
    <pre class="log">{logText}</pre>
  </Modal>
{/if}

<style>
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .spacer { flex: 1; }
  .grow { flex: 1; min-width: 0; }
  .small { font-size: 12.5px; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 12px; font-size: 13.5px; }
  .note.warning { background: var(--amber-soft); color: var(--amber-text); }
  .note.error, .note.err { background: var(--red-soft); color: var(--red-text); }
  .note :global(svg) { flex: none; margin-top: 1px; }
  .progline { display: flex; flex-wrap: wrap; gap: 6px 16px; align-items: baseline; font-size: 13.5px; }
  .runjob { margin-bottom: 16px; border-color: color-mix(in srgb, var(--accent) 40%, var(--border)); }
  .runjob .card-header > :global(svg) { color: var(--accent); }
  .devices { margin-bottom: 18px; }
  /* Let the cards shrink, so a wide table scrolls inside its card instead of
     widening the page on a phone (grid items don't shrink below their content). */
  .stack-v { grid-template-columns: minmax(0, 1fr); }
  .summary { display: grid; grid-template-columns: repeat(auto-fit, minmax(175px, 1fr)); gap: 14px; margin-bottom: 18px; }
  .tile { padding: 16px 18px; display: grid; gap: 8px; align-content: start; }
  .label { display: flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); font-weight: 500; }
  .value { font-size: 22px; font-weight: 600; letter-spacing: -0.02em; font-variant-numeric: tabular-nums; }
  .value small { font-size: 13px; font-weight: 400; color: var(--text-3); }
  .small-v { font-size: 17px; }
  .foot { font-size: 12.5px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2; -webkit-box-orient: vertical; }
  .btns { margin-top: 2px; flex-wrap: wrap; }
  .section { font-size: 15px; margin: 0; }
  .replink { text-decoration: none; }
  .replink:hover { text-decoration: none; filter: brightness(0.97); }
  .note.info { background: var(--surface-2); color: var(--text-2); }
  .note.success { background: var(--green-soft); color: var(--green-text); }
  .meter.big { height: 10px; }
  .meter.thin { height: 5px; }
  .green-t { color: var(--green-text); }
  .amber-t { color: var(--amber-text); }
  .red-t { color: var(--red-text); }
  .plain { margin: 0; padding-left: 18px; display: grid; gap: 4px; font-size: 13px; }
  .what { font-size: 13px; font-weight: 600; margin-bottom: 6px; }
  .kv { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: 8px 18px; margin: 0; font-size: 13.5px; }
  .kv dt { color: var(--text-3); }
  .kv dd { margin: 0; }
  p { margin: 0; }
  .chip { display: inline-flex; align-items: center; gap: 5px; padding: 2px 8px; border: 1px solid var(--border); border-radius: 99px; }
  .jobs { display: grid; }
  .job .grow { overflow: hidden; width: 0; }
  .job .grow .truncate { display: block; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .job { display: flex; gap: 12px; align-items: center; padding: 10px 18px; border-bottom: 1px solid var(--border); }
  .job:last-child { border-bottom: 0; }
  .job > :global(svg) { flex: none; }
  .job.ok > :global(svg) { color: var(--green); }
  .job.failed > :global(svg) { color: var(--red); }
  .job.stopped > :global(svg) { color: var(--amber); }
  .card-header.wrap { flex-wrap: wrap; }
  .tiny { height: 19px; font-size: 11px; margin-left: 8px; }
  .log { margin: 0; max-height: 60vh; overflow: auto; font-size: 12px; background: var(--surface-2); padding: 12px; border-radius: 8px; white-space: pre-wrap; overflow-wrap: anywhere; }
  button.danger { color: var(--red-text); }
  @media (max-width: 640px) { .summary { grid-template-columns: 1fr; } }
</style>
