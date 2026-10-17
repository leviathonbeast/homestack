<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import { ago, dateTime } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';

  let view = $state(null);
  let drafts = $state({});
  let error = $state('');
  let dirty = $state(false);
  let saving = $state(false);

  const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  const pad = (n) => String(n).padStart(2, '0');

  function toDraft(s) {
    return { weekdays: [0], day: 1, interval: 10, minute: 0, hour: 3, ...$state.snapshot(s), time: `${pad(s.hour || 0)}:${pad(s.minute || 0)}` };
  }

  async function load() {
    try {
      const v = await api.get('/host/schedules');
      view = v;
      if (!dirty) {
        const d = {};
        for (const t of v.tasks) d[t.id] = toDraft(t.schedule);
        drafts = d;
      }
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(() => {
    load();
    const t = setInterval(load, 30000);
    return () => clearInterval(t);
  });

  function change() {
    dirty = true;
  }
  function setEvery(id, every) {
    const d = drafts[id];
    d.every = every;
    if (every === 'weekly' && !d.weekdays?.length) d.weekdays = [0];
    if (every === 'monthly' && !d.day) d.day = 1;
    change();
  }
  function toggleDay(id, n) {
    const d = drafts[id];
    d.weekdays = d.weekdays.includes(n) ? d.weekdays.filter((x) => x !== n) : [...d.weekdays, n].sort();
    change();
  }
  function resetDefault(t) {
    drafts[t.id] = { ...toDraft(t.default), enabled: drafts[t.id].enabled };
    change();
  }

  function describe(d) {
    const at = d.time;
    switch (d.every) {
      case 'minutes': return `Every ${d.interval} minutes`;
      case 'hourly': return `Every hour at :${pad(d.minute)}`;
      case 'daily': return `Every day at ${at}`;
      case 'weekly': return `${d.weekdays.length === 7 ? 'Every day' : d.weekdays.map((n) => days[n]).join(', ')} at ${at}`;
      case 'monthly': return `Day ${d.day} of every month at ${at}`;
    }
    return '';
  }

  function payload() {
    return Object.values(drafts).map((d) => {
      const [h, m] = (d.time || '00:00').split(':').map(Number);
      return {
        id: d.id, enabled: !!d.enabled, every: d.every,
        interval: d.every === 'minutes' ? +d.interval : 0,
        minute: d.every === 'hourly' ? +d.minute : m || 0,
        hour: ['daily', 'weekly', 'monthly'].includes(d.every) ? h || 0 : 0,
        weekdays: d.every === 'weekly' ? d.weekdays : undefined,
        day: d.every === 'monthly' ? +d.day : 0,
      };
    });
  }

  async function save() {
    saving = true;
    try {
      view = await api.put('/host/schedules', { schedules: payload() });
      dirty = false;
      const d = {};
      for (const t of view.tasks) d[t.id] = toDraft(t.schedule);
      drafts = d;
      toast('Schedules saved', 'success', 2500);
    } catch (e) {
      toast(e.message, 'error', 9000);
    }
    saving = false;
  }
  function discard() {
    dirty = false;
    load();
  }
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Schedules</h1>
      <p class="sub">When the server's checks, mover and parity jobs run. Times are the server's ({view?.timezone || 'local'}).</p>
    </div>
    <div class="row">
      {#if dirty}<button onclick={discard} disabled={saving}>Discard</button>{/if}
      <button class="primary" onclick={save} disabled={!dirty || saving}><Icon name="save" size={15} /> {dirty ? 'Save' : 'Saved'}</button>
    </div>
  </div>

  {#if error}
    <div class="card note err"><Icon name="alert" size={16} /> {error}</div>
  {:else if !view}
    <div class="skeleton" style="height:420px"></div>
  {:else}
    {#each view.warnings as w}
      <div class="note warning"><Icon name="info" size={16} /> {w}</div>
    {/each}

    <div class="tasks">
      {#each view.tasks as t (t.id)}
        {@const d = drafts[t.id]}
        {#if d}
          <div class="card task" class:off={!d.enabled}>
            <div class="top">
              <button class="toggle" role="switch" aria-checked={d.enabled} aria-label="Run {t.title} on a schedule"
                disabled={!d.enabled && !!t.blocked} title={!d.enabled && t.blocked ? t.blocked : ''}
                onclick={() => { d.enabled = !d.enabled; change(); }}>
                <span class="switch" class:on={d.enabled}></span>
              </button>
              <div class="grow">
                <div class="title"><strong>{t.title}</strong><code class="dim">{t.command}</code></div>
                <div class="desc">{t.desc}</div>
              </div>
            </div>

            {#if t.blocked && !d.enabled}
              <div class="blocked"><Icon name="lock" size={13} /> {t.blocked}</div>
            {/if}

            <div class="when">
              <div class="segmented">
                {#each [['minutes', 'Minutes'], ['hourly', 'Hourly'], ['daily', 'Daily'], ['weekly', 'Weekly'], ['monthly', 'Monthly']] as [k, label]}
                  {#if k !== 'minutes' || t.allowMinutes}
                    <button class:active={d.every === k} onclick={() => setEvery(t.id, k)}>{label}</button>
                  {/if}
                {/each}
              </div>
              <div class="row wrap fields">
                {#if d.every === 'minutes'}
                  <label class="inline">Every
                    <select bind:value={d.interval} onchange={change}>
                      {#each [5, 10, 15, 20, 30] as n}<option value={n}>{n}</option>{/each}
                    </select> minutes</label>
                {:else if d.every === 'hourly'}
                  <label class="inline">At minute
                    <input type="number" min="0" max="59" class="num" bind:value={d.minute} oninput={change} /></label>
                {:else}
                  {#if d.every === 'weekly'}
                    <div class="days">
                      {#each days as name, n}
                        <button class="day" class:active={d.weekdays.includes(n)} onclick={() => toggleDay(t.id, n)}>{name}</button>
                      {/each}
                    </div>
                  {:else if d.every === 'monthly'}
                    <label class="inline">On day
                      <input type="number" min="1" max="28" class="num" bind:value={d.day} oninput={change} /></label>
                  {/if}
                  <label class="inline">at <input type="time" bind:value={d.time} oninput={change} /></label>
                {/if}
              </div>
            </div>

            <div class="foot">
              <span>{describe(d)}</span>
              {#if d.enabled && !dirty && t.next}<span class="dim">· next {ago(t.next)} ({dateTime(t.next)})</span>{/if}
              {#if !d.enabled}<span class="dim">· off</span>{/if}
              {#if t.lastRun}
                <span class="dim" title={t.lastRun.summary}>· last run {ago(t.lastRun.finished)} {t.lastRun.ok ? '' : '(failed)'}</span>
              {/if}
              <span class="spacer"></span>
              <button class="sm ghost" onclick={() => resetDefault(t)}>Default</button>
            </div>
          </div>
        {/if}
      {/each}
    </div>

    <p class="dim small tail">
      Saved to <code>{view.file}</code>{view.managed ? '' : ' the first time you save (your current file is kept as it is until then)'}.
      Jobs that touch the disks share one lock, so a parity job never runs at the same time as the mover or an array stop; the later one waits.
      ZFS pool scrubs keep their own monthly schedule from Debian (<code>/etc/cron.d/zfsutils-linux</code>).
    </p>
  {/if}
</div>

<style>
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .grow { flex: 1; min-width: 0; }
  .spacer { flex: 1; }
  .small { font-size: 12.5px; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 12px; font-size: 13.5px; }
  .note.warning { background: var(--amber-soft); color: var(--amber-text); }
  .note.err { background: var(--red-soft); color: var(--red-text); }
  .note :global(svg) { flex: none; margin-top: 1px; }
  .tasks { display: grid; grid-template-columns: repeat(auto-fill, minmax(440px, 1fr)); gap: 14px; }
  .task { padding: 16px 18px; display: grid; gap: 12px; align-content: start; }
  .task.off .when, .task.off .foot span:first-child { opacity: 0.6; }
  .top { display: flex; gap: 12px; align-items: flex-start; }
  .toggle { border: 0; background: none; padding: 3px 0 0; height: auto; }
  .toggle:disabled { opacity: 0.45; cursor: not-allowed; }
  .switch { display: block; width: 34px; height: 19px; border-radius: 10px; background: var(--border-strong); position: relative; }
  .switch::after { content: ''; position: absolute; top: 2px; left: 2px; width: 15px; height: 15px; border-radius: 50%; background: #fff; transition: transform 0.15s; }
  .switch.on { background: var(--accent); }
  .switch.on::after { transform: translateX(15px); }
  .title { display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap; }
  .title strong { font-size: 15px; }
  .title code { font-size: 11.5px; }
  .desc { font-size: 13px; color: var(--text-2); margin-top: 3px; }
  .blocked { display: flex; align-items: center; gap: 6px; font-size: 12.5px; color: var(--amber-text); background: var(--amber-soft); padding: 6px 10px; border-radius: 8px; }
  .when { display: grid; gap: 10px; }
  .segmented { justify-self: start; flex-wrap: wrap; }
  .fields { min-height: 32px; }
  .inline { display: inline-flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); }
  .inline select { width: 76px; height: 30px; }
  .inline input[type='time'] { height: 30px; width: 140px; }
  .num { width: 64px; height: 30px; }
  .days { display: inline-flex; gap: 4px; flex-wrap: wrap; }
  .day { height: 28px; padding: 0 9px; font-size: 12.5px; border-radius: 7px; }
  .day.active { background: var(--accent); border-color: var(--accent); color: #fff; }
  .foot { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; font-size: 12.5px; border-top: 1px solid var(--border); padding-top: 10px; }
  .tail { margin-top: 18px; line-height: 1.6; }
  @media (max-width: 560px) { .tasks { grid-template-columns: 1fr; } }
</style>
