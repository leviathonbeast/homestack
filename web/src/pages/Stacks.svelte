<script>
  import { api } from '../lib/api.js';
  import { live } from '../lib/state.svelte.js';
  import { stateColor, stackStatusLabel, portUrl, ago, appUrl, bytes, percent } from '../lib/format.js';
  import { stackAction, deleteStack } from '../lib/actions.js';
  import { navigate } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';

  let stacks = $state(null);
  let error = $state('');
  let search = $state('');
  let filter = $state('all');

  // List or grid (cards), remembered in this browser.
  let view = $state('list');
  try {
    view = localStorage.getItem('homestack-stacks-view') === 'grid' ? 'grid' : 'list';
  } catch {}
  function setView(v) {
    view = v;
    try {
      localStorage.setItem('homestack-stacks-view', v);
    } catch {}
  }

  $effect(() => {
    live.tick;
    api.get('/stacks').then((s) => ((stacks = s), (error = ''))).catch((e) => (error = e.message));
  });

  const managed = $derived((stacks || []).filter((s) => !s.external));
  const external = $derived((stacks || []).filter((s) => s.external));
  const shown = $derived(
    managed.filter((s) => {
      if (search && !s.name.toLowerCase().includes(search.toLowerCase())) return false;
      if (filter === 'running') return s.status === 'running' || s.status === 'partial';
      if (filter === 'stopped') return s.status === 'stopped' || s.status === 'inactive';
      if (filter === 'updates') return s.updateAvailable;
      return true;
    }),
  );
  const withUpdates = $derived(managed.filter((s) => s.updateAvailable));
  const counts = $derived({
    all: managed.length,
    running: managed.filter((s) => s.status === 'running' || s.status === 'partial').length,
    stopped: managed.filter((s) => s.status === 'stopped' || s.status === 'inactive').length,
    updates: managed.filter((s) => s.updateAvailable).length,
  });

  function webs(s) {
    const seen = new Set();
    return s.containers.filter((c) => c.webUI && !seen.has(c.webUI) && seen.add(c.webUI));
  }
  function ports(s) {
    const seen = new Set();
    const out = [];
    for (const c of s.containers) for (const p of c.ports) if (p.public && !seen.has(p.public)) (seen.add(p.public), out.push(p));
    return out;
  }
  function services(s) {
    return s.containers.map((c) => c.service || c.name);
  }
  function menuItems(s) {
    const up = s.status === 'running' || s.status === 'partial';
    return [
      { label: 'Open', icon: 'chevron-right', onclick: () => navigate(`/stacks/${s.name}`) },
      { label: 'Edit compose file', icon: 'code', onclick: () => navigate(`/stacks/${s.name}/editor`) },
      { label: 'Logs', icon: 'logs', onclick: () => navigate(`/stacks/${s.name}/logs`) },
      { divider: true },
      up ? { label: 'Restart', icon: 'restart', onclick: () => stackAction(s.name, 'restart') } : null,
      { label: 'Pull and update', icon: 'download', onclick: () => stackAction(s.name, 'update') },
      s.total ? { label: 'Take down', icon: 'power', hint: 'Remove containers, keep data', onclick: () => stackAction(s.name, 'down') } : null,
      { divider: true },
      { label: 'Delete stack', icon: 'trash', danger: true, onclick: () => deleteStack(s.name) },
    ];
  }
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Stacks</h1>
      <p class="sub">Each stack is a folder with a compose file. Start, stop, update and edit them here.</p>
    </div>
    <div class="row">
      <a class="btn primary" href="/stacks/new"><Icon name="plus" size={16} /> New stack</a>
    </div>
  </div>

  {#if withUpdates.length}
    <section class="card updates">
      <div class="card-header">
        <Icon name="upgrade" size={17} />
        <h2>Updates available</h2>
        <span class="badge amber">{withUpdates.length}</span>
      </div>
      {#each withUpdates as s (s.name)}
        <div class="urow">
          <a href="/stacks/{s.name}"><strong>{s.name}</strong></a>
          <span class="dim truncate">{[...new Set(s.containers.filter((x) => x.updateAvailable).map((x) => x.image))].join(', ')}</span>
          <span class="spacer"></span>
          <button class="sm primary" disabled={!!s.busy} onclick={() => stackAction(s.name, 'update')}><Icon name="download" size={14} /> Update</button>
        </div>
      {/each}
    </section>
  {/if}

  <div class="toolbar">
    <div class="search">
      <Icon name="search" size={15} />
      <input placeholder="Search stacks" bind:value={search} />
    </div>
    <div class="segmented">
      {#each [['all', 'All'], ['running', 'Running'], ['stopped', 'Stopped'], ['updates', 'Updates']] as [key, label]}
        <button class:active={filter === key} onclick={() => (filter = key)}>
          {label}{#if counts[key]}<span class="n">{counts[key]}</span>{/if}
        </button>
      {/each}
    </div>
    <span class="spacer"></span>
    <div class="segmented" role="group" aria-label="Layout">
      <button class:active={view === 'list'} onclick={() => setView('list')} title="List" aria-label="List"><Icon name="menu" size={15} /></button>
      <button class:active={view === 'grid'} onclick={() => setView('grid')} title="Grid" aria-label="Grid"><Icon name="dashboard" size={15} /></button>
    </div>
  </div>

  {#if error}<p class="error-text">{error}</p>{/if}

  {#if view === 'grid' && shown.length}
    <div class="sgrid">
      {#each shown as s (s.name)}
        {@const up = s.status === 'running' || s.status === 'partial'}
        <div class="card scard" role="link" tabindex="0" onclick={() => navigate(`/stacks/${s.name}`)} onkeydown={(e) => e.key === 'Enter' && e.target === e.currentTarget && navigate(`/stacks/${s.name}`)}>
          <div class="row">
            <span class="dot {stateColor(s.status)}"></span>
            <strong class="truncate grow">{s.name}</strong>
            <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
            <div class="acts" onclick={(e) => e.stopPropagation()}><Menu items={menuItems(s)} size="sm" /></div>
          </div>
          <div class="badges">
            <span class="muted small">{stackStatusLabel(s.status)}{s.total ? ` · ${s.running}/${s.total}` : ''}</span>
            {#if s.updateAvailable}<span class="badge amber tiny">Update</span>{/if}
            {#if s.busy}<span class="badge accent tiny"><Icon name="loader" size={11} class="spin" /> {s.busy}</span>{/if}
          </div>
          {#if s.containers.length}<div class="dim small truncate" title={services(s).join(', ')}>{services(s).join(', ')}</div>{/if}
          {#if s.cpu != null}
            <div class="usage small">
              <span title="CPU (100% = one core)"><Icon name="cpu" size={12} /> {percent(s.cpu)}</span>
              <span title="Memory"><Icon name="memory" size={12} /> {bytes(s.mem)}</span>
            </div>
          {/if}
          <div class="cfoot">
            <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
            <div class="ports" onclick={(e) => e.stopPropagation()}>
              {#if webs(s).length}
                {#each webs(s).slice(0, 3) as c}
                  <a class="port web" href={appUrl(c.webUI)} target="_blank" rel="noopener noreferrer" title={appUrl(c.webUI)}><Icon name="globe" size={11} /> {webs(s).length > 1 ? c.service || c.name : 'Open'}</a>
                {/each}
              {:else}
                {#each ports(s).slice(0, 3) as p}
                  <a class="port" href={portUrl(p)} target="_blank" rel="noreferrer">:{p.public}</a>
                {/each}
              {/if}
            </div>
            <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
            <div onclick={(e) => e.stopPropagation()}>
              {#if up}
                <button class="sm ghost icon" disabled={!!s.busy} onclick={() => stackAction(s.name, 'stop')} title="Stop" aria-label="Stop {s.name}"><Icon name="stop" size={14} /></button>
              {:else}
                <button class="sm ghost icon" disabled={!!s.busy} onclick={() => stackAction(s.name, 'up')} title="Start" aria-label="Start {s.name}"><Icon name="play" size={14} /></button>
              {/if}
            </div>
          </div>
        </div>
      {/each}
    </div>
  {:else}
  <div class="card">
    {#if stacks === null}
      {#each Array(3) as _}<div class="srow"><div class="skeleton" style="height:18px;width:40%"></div></div>{/each}
    {:else if !managed.length}
      <div class="empty">
        <h3>Create your first stack</h3>
        <p>Pick an app template like Jellyfin or Pi-hole, or paste your own compose file.</p>
        <div class="row" style="justify-content:center">
          <a class="btn primary" href="/stacks/new"><Icon name="plus" size={16} /> New stack</a>
        </div>
      </div>
    {:else if !shown.length}
      <div class="empty"><p>No stacks match.</p></div>
    {:else}
      {#each shown as s (s.name)}
        {@const up = s.status === 'running' || s.status === 'partial'}
        <div class="srow" role="link" tabindex="0" onclick={() => navigate(`/stacks/${s.name}`)} onkeydown={(e) => e.key === 'Enter' && e.target === e.currentTarget && navigate(`/stacks/${s.name}`)}>
          <span class="dot {stateColor(s.status)}"></span>
          <div class="main">
            <div class="row">
              <strong>{s.name}</strong>
              {#if s.updateAvailable}<span class="badge amber">Update available</span>{/if}
              {#if s.busy}<span class="badge accent"><Icon name="loader" size={12} class="spin" /> {s.busy}</span>{/if}
            </div>
            <div class="meta">
              <span>{stackStatusLabel(s.status)}{s.total ? ` · ${s.running} of ${s.total} running` : ''}</span>
              {#if s.cpu != null}<span class="dim nowrap hide-sm" title="CPU (100% = one core) and memory">{percent(s.cpu)} · {bytes(s.mem)}</span>{/if}
              {#if s.containers.length}<span class="dim truncate hide-sm">{services(s).join(', ')}</span>{/if}
              {#if s.modified}<span class="dim hide-sm nowrap">edited {ago(s.modified)}</span>{/if}
            </div>
          </div>
          <div class="ports hide-sm">
            {#if webs(s).length}
              {#each webs(s).slice(0, 3) as c}
                <a class="port web" href={appUrl(c.webUI)} target="_blank" rel="noopener noreferrer" title={appUrl(c.webUI)} onclick={(e) => e.stopPropagation()}><Icon name="globe" size={11} /> {webs(s).length > 1 ? c.service || c.name : 'Open'}</a>
              {/each}
            {:else}
              {#each ports(s).slice(0, 4) as p}
                <a class="port" href={portUrl(p)} target="_blank" rel="noreferrer" onclick={(e) => e.stopPropagation()}>:{p.public} <Icon name="external" size={11} /></a>
              {/each}
            {/if}
          </div>
          <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
          <div class="acts" onclick={(e) => e.stopPropagation()}>
            {#if up}
              <button class="sm" disabled={!!s.busy} onclick={() => stackAction(s.name, 'stop')} title="Stop"><Icon name="stop" size={14} /><span class="hide-sm">Stop</span></button>
            {:else}
              <button class="sm" disabled={!!s.busy} onclick={() => stackAction(s.name, 'up')} title="Start"><Icon name="play" size={14} /><span class="hide-sm">Start</span></button>
            {/if}
            <Menu items={menuItems(s)} size="sm" />
          </div>
        </div>
      {/each}
    {/if}
  </div>
  {/if}

  {#if external.length}
    <h2 class="ext-title">Started outside Homestack</h2>
    <p class="muted ext-sub">These Compose projects aren't in your stacks folder. You can manage their containers, but not edit or redeploy them here. Move the folder into your stacks folder to manage it fully.</p>
    <div class="card">
      {#each external as s (s.name)}
        <div class="srow" role="link" tabindex="0" onclick={() => navigate(`/stacks/${s.name}`)} onkeydown={(e) => e.key === 'Enter' && navigate(`/stacks/${s.name}`)}>
          <span class="dot {stateColor(s.status)}"></span>
          <div class="main">
            <strong>{s.name}</strong>
            <div class="meta"><span>{s.running} of {s.total} running</span>{#if s.dir}<span class="dim mono truncate">{s.dir}</span>{/if}</div>
          </div>
          <Icon name="chevron-right" size={16} class="dim" />
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .updates { margin-bottom: 14px; border-color: color-mix(in srgb, var(--amber) 35%, var(--border)); }
  .updates .card-header :global(svg) { color: var(--amber); }
  .urow { display: flex; align-items: center; gap: 12px; padding: 10px 18px; border-bottom: 1px solid var(--border); min-width: 0; }
  .urow:last-child { border-bottom: 0; }
  .urow .spacer { flex: 1; }
  .toolbar { display: flex; gap: 10px; margin-bottom: 14px; flex-wrap: wrap; }
  .toolbar .search { flex: 1 1 240px; max-width: 360px; }
  .n { margin-left: 6px; font-size: 11.5px; color: var(--text-3); }
  .srow { display: flex; align-items: center; gap: 14px; padding: 14px 16px 14px 18px; border-bottom: 1px solid var(--border); cursor: pointer; transition: background 0.1s; }
  .srow:last-child { border-bottom: 0; }
  .srow:hover { background: var(--surface-2); }
  .srow:first-child { border-radius: 12px 12px 0 0; }
  .srow:last-child { border-radius: 0 0 12px 12px; }
  .main { flex: 1; min-width: 0; display: grid; gap: 2px; }
  .main strong { font-size: 14.5px; }
  .meta { display: flex; gap: 12px; font-size: 12.5px; color: var(--text-2); min-width: 0; }
  .ports { display: flex; gap: 5px; flex-wrap: wrap; justify-content: flex-end; max-width: 280px; }
  .acts { display: flex; gap: 4px; align-items: center; }
  .sgrid { display: grid; grid-template-columns: repeat(auto-fill, minmax(250px, 1fr)); gap: 12px; }
  .scard { padding: 14px 14px 10px 16px; display: grid; gap: 6px; align-content: start; cursor: pointer; min-width: 0; transition: border-color 0.12s, box-shadow 0.12s; }
  .scard:hover { border-color: color-mix(in srgb, var(--accent) 40%, var(--border)); box-shadow: var(--shadow-lg); }
  .scard:focus-visible { outline: 2px solid var(--accent); }
  .scard strong { font-size: 14.5px; }
  .grow { flex: 1; min-width: 0; }
  .badges { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; }
  .tiny { height: 19px; font-size: 11px; padding: 0 7px; }
  .small { font-size: 12.5px; }
  .usage { display: flex; gap: 14px; color: var(--text-2); font-variant-numeric: tabular-nums; }
  .usage span { display: inline-flex; align-items: center; gap: 4px; }
  .usage :global(svg) { color: var(--text-3); }
  .cfoot { display: flex; align-items: center; gap: 8px; margin-top: 2px; min-height: 28px; }
  .cfoot .ports { flex: 1; justify-content: flex-start; max-width: none; }
  .ext-title { margin: 28px 0 4px; font-size: 15px; }
  .ext-sub { font-size: 13px; margin-bottom: 12px; max-width: 720px; }
</style>
