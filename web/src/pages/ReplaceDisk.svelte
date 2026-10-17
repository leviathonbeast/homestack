<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast, confirm } from '../lib/state.svelte.js';
  import { bytes, ago, dateTime, duration } from '../lib/format.js';
  import { query } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from '../components/Modal.svelte';

  let v = $state(null);
  let error = $state('');
  let busy = $state(false);
  let pick = $state(query('disk') || '');
  let dev = $state('');
  let fixPerms = $state(true);
  let typed = $state('');
  let logOpen = $state(false);
  let logText = $state('');
  let showUnrec = $state(false);

  async function load() {
    try {
      v = await api.get('/host/replace');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(() => {
    let t;
    const tick = async () => {
      await load();
      // Quick while a job runs; slower otherwise (the view scans the disks).
      t = setTimeout(tick, v?.op && ['running'].includes(v.op.state) ? 3000 : 15000);
    };
    tick();
    return () => clearTimeout(t);
  });

  const op = $derived(v?.op);
  const slot = $derived(v?.slots.find((s) => s.pool === pick));
  const cand = $derived(v?.candidates.find((c) => c.device === dev));
  const running = $derived(v?.running?.action === 'disk-replace' ? v.running : null);

  const stateText = { ONLINE: 'Working', MISSING: 'Missing', IMPORTABLE: 'Not imported', FAULTED: 'Faulted', UNAVAIL: 'Unavailable', SUSPENDED: 'Suspended', DEGRADED: 'Degraded', REMOVED: 'Removed', OFFLINE: 'Offline' };
  const methodTitle = { rebuild: 'Rebuild from parity', copy: 'Copy to the new disk', 'add-data': 'New data disk', 'add-parity': 'New parity disk', 'replace-parity': 'Parity to a new disk', 'rebuild-parity': 'Rebuild parity' };
  // Adding a disk or moving parity (Storage → Array devices) runs here too.
  const adding = (o) => o.method.startsWith('add-');
  const opTitle = (o) => `${adding(o) ? 'Adding' : 'Replacing'} ${o.pool}`;
  const assigning = $derived(op && ['add-data', 'add-parity', 'replace-parity', 'rebuild-parity'].includes(op.method));
  const cancelText = {
    'add-data': 'It\'s taken back out of the shares and SnapRAID and its pool is destroyed, as long as no files have been written to it yet. Otherwise this is refused.',
    'add-parity': 'It\'s taken back out of SnapRAID and /etc/fstab. The disk keeps an empty filesystem and is unassigned.',
    'replace-parity': 'Before the swap, parity stays on its old disk. After it, the new disk stays parity.',
    'rebuild-parity': 'Before the new disk takes over, nothing has changed on the array. After that it\'s refused: only finishing brings parity back.',
  };

  // Why a disk can't be the new one for the picked slot (beyond the agent's own checks).
  function unsuitable(c) {
    if (c.blocked) return c.blocked;
    if (!slot) return '';
    if (slot.method === 'copy' && c.size < slot.size) return `Smaller than ${slot.pool} (${bytes(slot.size, 1)}). Copying needs a disk at least as big.`;
    if (slot.method === 'rebuild' && slot.used && c.size * 0.95 < slot.used) return `Too small for the ${bytes(slot.used, 1)} that was on ${slot.pool}.`;
    return '';
  }
  const biggerThanParity = (c) => v?.paritySize && c.size > v.paritySize;

  // Rough time: SnapRAID reads ~150 MB/s on these disks; fix + check read it twice.
  function estimate(s) {
    if (!s?.used) return '';
    const secs = s.method === 'rebuild' ? (s.used / 150e6) * 2 : s.used / 170e6;
    const h = Math.round(secs / 3600);
    return h < 1 ? 'under an hour' : h < 36 ? `about ${h} hours` : `about ${Math.round(h / 24)} days`;
  }

  function choose(s) {
    if (!s.method) return;
    pick = s.pool;
    dev = '';
    typed = '';
    history.replaceState({}, '', '/storage/replace?disk=' + encodeURIComponent(s.pool));
  }

  async function post(body, msg) {
    busy = true;
    try {
      v = await api.post('/host/replace', body);
      if (msg) toast(msg, 'success', 3000);
    } catch (e) {
      toast(e.message, 'error', 10000);
    }
    busy = false;
  }
  function start() {
    post({ action: 'start', method: slot.method, snap: slot.snap, device: cand.device, confirm: typed.trim(), fixPerms }, `Replacing ${slot.pool}`);
  }
  async function stop() {
    const copy = op.method === 'copy';
    const ok = await confirm({
      title: 'Stop for now?',
      message: copy ? 'This only stops watching: ZFS carries on copying in the background. Continue picks it up again.' : 'The current step stops cleanly and Continue carries on from where it stopped. The mover and parity jobs stay paused meanwhile.',
      confirmLabel: 'Stop',
    });
    if (!ok) return;
    try {
      await api.post('/host/actions', { action: 'job-stop' });
      await load();
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
  }
  async function accept() {
    const ok = await confirm({
      title: 'Accept and finish?',
      message: 'The files that couldn\'t be rebuilt are given up: the next parity sync drops them, so parity can\'t bring them back afterwards. Only do this once you\'re sure no other disk can supply them.',
      confirmLabel: 'Accept', danger: true, typeToConfirm: 'ACCEPT',
    });
    if (ok) post({ action: 'accept', confirm: 'ACCEPT' });
  }
  async function cancel() {
    const ok = await confirm({
      title: `Cancel ${opTitle(op).toLowerCase()}?`,
      message: cancelText[op.method] || (op.method === 'copy' ? 'The copy is stopped and the new disk is detached. The old disk carries on as before.' : `Nothing more is done. ${op.pool} stays missing until it's replaced; parity still covers its files.`),
      confirmLabel: 'Cancel it', danger: true,
    });
    if (ok) post({ action: 'cancel' });
  }
  async function showLog() {
    const j = v.lastJob;
    if (!j) return;
    logOpen = true;
    logText = 'Loading…';
    try {
      const r = await fetch(`/api/host/jobs/${j.id}/log`, { credentials: 'same-origin' });
      logText = r.ok ? await r.text() : 'No log.';
    } catch {
      logText = 'Couldn\'t load the log.';
    }
  }

  function etaText(sec) {
    if (sec < 60) return 'under a minute';
    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60);
    return h ? `${h}h ${String(m).padStart(2, '0')}m` : `${m} min`;
  }
  function finishAt(sec) {
    const t = new Date(Date.now() + sec * 1000);
    const same = t.toDateString() === new Date().toDateString();
    const time = t.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    return same ? time : `${t.toLocaleDateString([], { weekday: 'short' })} ${time}`;
  }
  const stepIcon = { done: 'check', running: 'loader', attention: 'alert', failed: 'alert', skipped: 'minimize', pending: '' };
  const took = (s) => (s.started && s.finished ? duration((new Date(s.finished) - new Date(s.started)) / 1000) : '');
  const opBanner = {
    running: ['info', 'Working on it. You can leave this page; progress and the result also go to your notifications.'],
    paused: ['warning', ''], attention: ['error', ''], failed: ['error', ''], done: ['success', ''], cancelled: ['info', ''],
  };
</script>

<div class="page">
  <div class="page-header">
    <div>
      <a class="back" href="/storage"><Icon name="chevron-left" size={14} /> Storage</a>
      <h1>{assigning ? 'Array devices' : 'Replace a disk'}</h1>
      <p class="sub">{assigning ? 'Adding a disk, or moving parity to a new one.' : 'For a failed disk, or one you\'re upgrading to a bigger one.'}</p>
    </div>
  </div>

  {#if error}
    <div class="note error"><Icon name="alert" size={16} /> {error}</div>
  {:else if !v}
    <div class="skeleton" style="height:420px"></div>
  {:else if op}
    <!-- ================= A replacement in progress (or just finished) ================= -->
    <section class="card">
      <div class="card-header">
        <h2>{op.state === 'done' ? `${op.pool} ${adding(op) ? 'added' : 'replaced'}` : op.state === 'cancelled' ? `${opTitle(op)}: cancelled` : opTitle(op)}</h2>
        <span class="badge">{methodTitle[op.method]}</span>
        <span class="spacer"></span>
        <span class="dim small">started {dateTime(op.started)}</span>
      </div>
      <div class="card-body stack-v">
        <div class="swap">
          {#if !adding(op)}
            <div class="disk old">
              <div class="dim small">Old</div>
              <strong>{op.oldModel || 'Unknown disk'}</strong>
              <div class="dim small mono">{op.oldSerial || '–'}{op.oldSize ? ` · ${bytes(op.oldSize, 1)}` : ''}</div>
            </div>
            <Icon name="chevron-right" size={18} />
          {/if}
          <div class="disk new">
            <div class="dim small">New</div>
            <strong>{op.newModel}</strong>
            <div class="dim small mono">{op.newSerial} · {bytes(op.newSize, 1)}</div>
          </div>
        </div>

        {#if op.message || opBanner[op.state]?.[1]}
          <div class="note {opBanner[op.state]?.[0] || 'info'}"><Icon name={op.state === 'done' ? 'check' : op.state === 'running' ? 'info' : 'alert'} size={16} /> <span>{op.message || opBanner[op.state][1]}</span></div>
        {/if}

        <ol class="steps">
          {#each op.steps as s, i (s.key)}
            <li class="step {s.state}">
              <span class="mark">{#if stepIcon[s.state]}<Icon name={stepIcon[s.state]} size={14} class={s.state === 'running' ? 'spin' : ''} />{:else}{i + 1}{/if}</span>
              <div class="grow">
                <div class="row"><strong>{s.title}</strong><span class="spacer"></span>{#if took(s)}<span class="dim small">{took(s)}</span>{/if}</div>
                {#if s.state === 'running' && running}
                  {#if running.progress != null}
                    <div class="meter"><div style="width:{running.progress}%"></div></div>
                    <div class="progline small">
                      <strong>{running.progress.toFixed ? running.progress.toFixed(running.progress < 10 ? 1 : 0) : running.progress}%</strong>
                      {#if running.doneBytes != null}<span>{running.totalBytes ? `${bytes(running.doneBytes)} of ${bytes(running.totalBytes)}` : `${bytes(running.doneBytes)} read`}</span>{/if}
                      {#if running.speed}<span class="dim">{running.speed}</span>{/if}
                      {#if running.etaSeconds != null}<span><strong>{etaText(running.etaSeconds)}</strong> to go</span><span class="dim">finishes around {finishAt(running.etaSeconds)}</span>{:else if running.eta}<span>{running.eta} left</span>{/if}
                    </div>
                  {:else if running.step}
                    <div class="dim small">{running.step}</div>
                  {/if}
                {:else if s.detail}
                  <div class="small detail">{s.detail}</div>
                {/if}
              </div>
            </li>
          {/each}
        </ol>

        {#if op.unrecoverable?.length}
          <div>
            <button class="sm ghost" onclick={() => (showUnrec = !showUnrec)}><Icon name={showUnrec ? 'chevron-down' : 'chevron-right'} size={13} /> {op.unrecoverable.length} file{op.unrecoverable.length === 1 ? '' : 's'} couldn't be rebuilt</button>
            {#if showUnrec}<pre class="log">{op.unrecoverable.join('\n')}</pre>{/if}
          </div>
        {/if}

        <div class="row wrap">
          {#if op.state === 'running'}
            <button onclick={stop} disabled={busy || !running}><Icon name="stop" size={15} /> Stop</button>
          {:else if op.state === 'paused' || op.state === 'failed'}
            <button class="primary" onclick={() => post({ action: 'continue' })} disabled={busy}><Icon name="play" size={15} /> Continue</button>
            <button class="ghost" onclick={cancel} disabled={busy}>Cancel</button>
          {:else if op.state === 'attention'}
            <button class="primary" onclick={() => post({ action: 'continue' })} disabled={busy}><Icon name="refresh" size={15} /> Try again</button>
            <button class="danger" onclick={accept} disabled={busy}>Accept and finish</button>
            <button class="ghost" onclick={cancel} disabled={busy}>Cancel</button>
          {:else}
            <button class="primary" onclick={() => post({ action: 'dismiss' })} disabled={busy}><Icon name="check" size={15} /> Done</button>
          {/if}
          {#if v.lastJob}<button class="ghost" onclick={showLog}><Icon name="logs" size={15} /> Log</button>{/if}
          <span class="spacer"></span>
          {#if op.method === 'copy' && op.state === 'done'}<span class="dim small">The old disk ({op.oldSerial}) is no longer used and can be removed.</span>{/if}
        </div>
      </div>
    </section>

    {#if op.createArgs?.length && (op.method === 'rebuild' || op.method === 'add-data')}
      <p class="dim small tail">The new pool is created with <code>zpool create {op.createArgs.join(' ')} -m {op.path} {op.pool} {op.newDevice}</code> (settings copied from {op.template}).</p>
    {/if}
    {#if op.backups?.length}
      <p class="dim small tail">The files as they were before this job: {#each op.backups as b, i}{i ? ', ' : ''}<code>{b}</code>{/each}.</p>
    {/if}
  {:else}
    <!-- ================= The wizard ================= -->
    {#if v.busy}<div class="note warning"><Icon name="info" size={16} /> Busy: {v.busy} is running. Starting waits for it to finish.</div>{/if}

    <section class="card">
      <div class="card-header"><span class="num">1</span><h2>Which disk?</h2></div>
      <div class="list">
        {#each v.slots as s (s.snap)}
          <button class="opt" class:sel={pick === s.pool} disabled={!s.method} onclick={() => choose(s)}>
            <span class="dot {s.state === 'ONLINE' && s.mounted ? 'green' : s.state === 'ONLINE' ? 'gray' : 'red'}"></span>
            <div class="grow">
              <div class="row"><strong>{s.pool}</strong><span class="badge {s.state === 'ONLINE' ? '' : 'red'}">{stateText[s.state] || s.state}</span>{#if s.smartFailing}<span class="badge red">SMART failing</span>{/if}<span class="spacer"></span><span class="dim small">{s.used ? `${bytes(s.used, 1)} used` : ''}</span></div>
              <div class="dim small">{s.model || 'Unknown disk'}{s.serial ? ` · ${s.serial}` : ''}{s.size ? ` · ${bytes(s.size, 1)}` : ''}{s.remembered ? ' (as last seen)' : ''}</div>
              {#if !s.method}<div class="small why">{s.why}</div>{/if}
            </div>
          </button>
        {:else}
          <div class="card-body dim">No data disks in snapraid.conf.</div>
        {/each}
      </div>
    </section>

    {#if slot?.method}
      <section class="card">
        <div class="card-header"><span class="num">2</span><h2>{methodTitle[slot.method]}</h2></div>
        <div class="card-body stack-v">
          <p class="lead">{slot.why}</p>
          <ul class="checks">
            {#if slot.method === 'rebuild'}
              <li><Icon name="check" size={14} /> Parity was last synced {v.lastSync ? ago(v.lastSync) : 'never'}{v.lastSync ? ` (${dateTime(v.lastSync)})` : ''}. Files put on {slot.pool} after that can't be rebuilt.</li>
              <li><Icon name="check" size={14} /> Every other disk is working, and parity is mounted.</li>
              <li><Icon name="info" size={14} /> Best to pause apps that write to the shares (download clients) until it's done.</li>
            {:else}
              <li><Icon name="check" size={14} /> {slot.pool} stays in use while it copies: shares and containers carry on.</li>
              <li><Icon name="info" size={14} /> Connect the new disk alongside the old one (a free SATA port), then press Refresh below.</li>
            {/if}
            <li><Icon name="clock" size={14} /> Takes {estimate(slot) || 'a while'} for {bytes(slot.used, 1)}. The mover and parity jobs are paused until it's done.</li>
          </ul>
        </div>
      </section>

      <section class="card">
        <div class="card-header"><span class="num">3</span><h2>The new disk</h2><span class="spacer"></span><button class="sm ghost" onclick={load}><Icon name="refresh" size={13} /> Refresh</button></div>
        <div class="list">
          {#each v.candidates as c (c.name)}
            {@const why = unsuitable(c)}
            <button class="opt" class:sel={dev === c.device} disabled={!!why} onclick={() => { dev = c.device; typed = ''; }}>
              <Icon name="disk" size={18} />
              <div class="grow">
                <div class="row"><strong>{c.model}</strong><span class="dim small mono">{c.serial}</span><span class="spacer"></span><strong>{bytes(c.size, 1)}</strong></div>
                <div class="dim small">/dev/{c.name} · {c.transport?.toUpperCase()} · {c.contents}</div>
                {#if why}<div class="small why">{why}</div>{:else}
                  {#if c.warn}<div class="small amber-t">{c.warn}</div>{/if}
                  {#if biggerThanParity(c)}<div class="small amber-t">Bigger than the parity disk ({bytes(v.paritySize, 1)}). That's fine while {slot.pool} holds less than that; to use all of it, upgrade parity first.</div>{/if}
                {/if}
              </div>
            </button>
          {:else}
            <div class="card-body dim small">No spare disk found. Connect the new disk{slot.method === 'rebuild' ? ' in place of the old one' : ''} and press Refresh. Shut the server down first unless the bay is hot-swap. USB disks aren't offered.</div>
          {/each}
        </div>
      </section>

      {#if cand && !unsuitable(cand)}
        <section class="card">
          <div class="card-header"><span class="num">4</span><h2>Check and start</h2></div>
          <div class="card-body stack-v">
            <div class="swap">
              <div class="disk old"><div class="dim small">{slot.pool} now</div><strong>{slot.model || 'Unknown disk'}</strong><div class="dim small mono">{slot.serial || '–'}</div></div>
              <Icon name="chevron-right" size={18} />
              <div class="disk new"><div class="dim small">{slot.pool} after</div><strong>{cand.model}</strong><div class="dim small mono">{cand.serial} · {bytes(cand.size, 1)}</div></div>
            </div>
            <ol class="plan small">
              {#if slot.method === 'rebuild'}
                <li>Pause the mover and parity jobs.</li>
                <li>Remove the old {slot.pool} pool record{slot.state === 'MISSING' ? ' (already gone)' : ''}.</li>
                <li><strong>Erase</strong> {cand.model} {cand.serial} and create {slot.pool} on it, mounted at {slot.path}, with the same settings as your other disks.</li>
                <li>Rebuild every file from parity (<code>snapraid fix -d {slot.snap}</code>), then check each one against its checksum.</li>
                <li>{#if fixPerms}Set owners and permissions to {v.owner || 'nobody:users'} (SnapRAID doesn't keep them).{:else}Leave owners as restored (root).{/if}</li>
                <li>Sync parity, then resume the mover and parity jobs.</li>
              {:else}
                <li>Pause the mover and parity jobs.</li>
                <li><strong>Erase</strong> {cand.model} {cand.serial}.</li>
                <li>Copy {slot.pool} onto it with <code>zpool replace</code>; {slot.pool} stays in use.</li>
                <li>Grow {slot.pool} to the new disk's size and release the old disk, then resume the jobs.</li>
              {/if}
            </ol>
            {#if slot.method === 'rebuild'}
              <label class="check"><input type="checkbox" bind:checked={fixPerms} /> Reset owners and permissions afterwards ({v.owner || 'nobody:users'}, like Unraid's New Permissions)</label>
            {/if}
            <label class="field">
              <span>Type the new disk's serial number, <strong class="mono">{cand.serial}</strong>, to confirm it may be erased</span>
              <input bind:value={typed} autocomplete="off" spellcheck="false" class="mono" placeholder={cand.serial} />
            </label>
            <div class="row">
              <button class="primary" onclick={start} disabled={busy || typed.trim() !== cand.serial}><Icon name="play" size={15} /> Start {slot.method === 'rebuild' ? 'the rebuild' : 'the copy'}</button>
              <span class="dim small">You can leave the page; it carries on (and picks up again after a restart).</span>
            </div>
          </div>
        </section>
      {/if}
    {/if}

    <p class="dim small tail">
      To add a disk rather than replace one, or to move parity to a bigger disk, use <a href="/storage">Array devices</a> on the Storage page.
      Parity must be at least as big as the fullest data disk{v.paritySize ? ` (yours is ${bytes(v.paritySize, 1)})` : ''}.
    </p>
  {/if}
</div>

{#if logOpen}
  <Modal title="Replacement log" width={860} onclose={() => (logOpen = false)}>
    <pre class="log big">{logText}</pre>
  </Modal>
{/if}

<style>
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .spacer { flex: 1; }
  .grow { flex: 1; min-width: 0; }
  .small { font-size: 12.5px; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  .back { display: inline-flex; align-items: center; gap: 3px; font-size: 13px; color: var(--text-3); margin-bottom: 4px; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; font-size: 13.5px; }
  .note.warning { background: var(--amber-soft); color: var(--amber-text); }
  .note.error { background: var(--red-soft); color: var(--red-text); }
  .note.success { background: var(--green-soft); color: var(--green-text); }
  .note.info { background: var(--surface-2); color: var(--text-2); }
  .note :global(svg) { flex: none; margin-top: 1px; }
  section.card { margin-bottom: 16px; }
  .num { display: inline-grid; place-items: center; width: 22px; height: 22px; border-radius: 50%; background: var(--accent); color: var(--on-accent, #fff); font-size: 12px; font-weight: 600; flex: none; }
  .list { display: grid; grid-template-columns: minmax(0, 1fr); }
  .opt .row { flex-wrap: wrap; row-gap: 2px; }
  .opt .dim { overflow-wrap: anywhere; }
  .swap .disk { min-width: 0; }
  @media (max-width: 560px) { .swap > :global(svg) { display: none; } .disk { max-width: none; flex-basis: 100%; } .opt { padding: 12px 14px; } }
  .opt { white-space: normal; display: flex; gap: 12px; align-items: flex-start; text-align: left; height: auto; padding: 12px 18px; border: 0; border-bottom: 1px solid var(--border); border-radius: 0; background: none; width: 100%; }
  .opt:last-child { border-bottom: 0; }
  .opt:not(:disabled):hover { background: var(--surface-2); }
  .opt.sel { background: color-mix(in srgb, var(--accent) 12%, transparent); box-shadow: inset 3px 0 0 var(--accent); }
  .opt:disabled { cursor: default; opacity: 1; }
  .opt:disabled strong, .opt:disabled .dim { opacity: 0.7; }
  .opt > :global(svg) { flex: none; margin-top: 2px; color: var(--text-3); }
  .opt .dot { margin-top: 6px; }
  .why { color: var(--amber-text); margin-top: 4px; line-height: 1.45; }
  .amber-t { color: var(--amber-text); margin-top: 3px; }
  .lead { margin: 0; font-size: 13.5px; line-height: 1.5; }
  .checks { list-style: none; margin: 0; padding: 0; display: grid; gap: 7px; font-size: 13px; }
  .checks li { display: flex; gap: 8px; align-items: flex-start; line-height: 1.45; }
  .checks :global(svg) { flex: none; margin-top: 2px; color: var(--text-3); }
  .swap { display: flex; gap: 14px; align-items: center; flex-wrap: wrap; }
  .swap > :global(svg) { color: var(--text-3); }
  .disk { padding: 10px 14px; border: 1px solid var(--border); border-radius: 10px; min-width: 200px; flex: 1; max-width: 340px; }
  .disk.old { background: var(--surface-2); }
  .disk.new { border-color: color-mix(in srgb, var(--accent) 50%, var(--border)); }
  .plan { margin: 0; padding-left: 20px; display: grid; gap: 5px; line-height: 1.45; }
  .check { display: flex; gap: 8px; align-items: center; font-size: 13px; }
  .field { display: grid; gap: 6px; font-size: 13px; }
  .field input { max-width: 340px; }
  .steps { list-style: none; margin: 0; padding: 0; display: grid; gap: 2px; }
  .step { display: flex; gap: 12px; align-items: flex-start; padding: 9px 0; border-bottom: 1px solid var(--border); }
  .step:last-child { border-bottom: 0; }
  .mark { width: 24px; height: 24px; border-radius: 50%; display: grid; place-items: center; flex: none; font-size: 12px; font-weight: 600; background: var(--surface-2); color: var(--text-3); }
  .step.done .mark { background: var(--green-soft); color: var(--green-text); }
  .step.running .mark { background: color-mix(in srgb, var(--accent) 18%, transparent); color: var(--accent); }
  .step.attention .mark, .step.failed .mark { background: var(--red-soft); color: var(--red-text); }
  .step.skipped { opacity: 0.6; }
  .step.pending strong { color: var(--text-2); font-weight: 500; }
  .step .meter { margin-top: 7px; height: 8px; }
  .detail { color: var(--text-2); margin-top: 2px; line-height: 1.45; overflow-wrap: anywhere; }
  .step.attention .detail, .step.failed .detail { color: var(--red-text); }
  .progline { display: flex; flex-wrap: wrap; gap: 4px 14px; align-items: baseline; margin-top: 6px; }
  .log { margin: 8px 0 0; max-height: 260px; overflow: auto; font-size: 12px; background: var(--surface-2); padding: 10px 12px; border-radius: 8px; white-space: pre-wrap; overflow-wrap: anywhere; }
  .log.big { max-height: 62vh; margin: 0; }
  button.danger { color: var(--red-text); }
  .tail { margin-top: 6px; line-height: 1.6; }
  .tail code { overflow-wrap: anywhere; }
</style>
