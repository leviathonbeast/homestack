<script>
  import { api, subscribe } from '../lib/api.js';
  import { live, toast, runJob } from '../lib/state.svelte.js';
  import { bytes, percent, stateColor, stackStatusLabel, containerStateLabel, portUrl, appUrl } from '../lib/format.js';
  import { stackAction, deleteStack, containerAction } from '../lib/actions.js';
  import { navigate } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';
  import LogViewer from '../components/LogViewer.svelte';
  import CodeEditor from '../components/CodeEditor.svelte';

  let { name, tab = 'overview' } = $props();

  let stack = $state(null);
  let notFound = $state(false);
  let stats = $state({});

  $effect(() => {
    live.tick;
    api
      .get(`/stacks/${encodeURIComponent(name)}`)
      .then((s) => (stack = s))
      .catch((e) => {
        if (e.status === 404) notFound = true;
        else toast(e.message, 'error');
      });
  });

  // Live CPU and memory while the overview is open.
  $effect(() => {
    if (tab !== 'overview') return;
    return subscribe(`/stacks/${encodeURIComponent(name)}/stats`, { stats: (s) => (stats = s) });
  });

  const up = $derived(stack && (stack.status === 'running' || stack.status === 'partial'));
  const totalCpu = $derived(Object.values(stats).reduce((a, s) => a + s.cpu, 0));
  const totalMem = $derived(Object.values(stats).reduce((a, s) => a + s.mem, 0));
  const totalNet = $derived(Object.values(stats).reduce((a, s) => a + s.netRx + s.netTx, 0));

  const tabs = $derived([
    { id: 'overview', label: 'Overview', icon: 'dashboard' },
    { id: 'logs', label: 'Logs', icon: 'logs' },
    ...(stack?.external ? [] : [{ id: 'editor', label: 'Compose file', icon: 'code' }]),
  ]);

  // ---------- Editor ----------
  let compose = $state('');
  let env = $state('');
  let saved = $state({ compose: '', env: '' });
  let composeFile = $state('compose.yaml');
  let loadedFiles = $state(false);
  let validation = $state(null);
  let saving = $state(false);
  const dirty = $derived(loadedFiles && (compose !== saved.compose || env !== saved.env));

  $effect(() => {
    if (tab !== 'editor' || loadedFiles || stack?.external) return;
    api
      .get(`/stacks/${encodeURIComponent(name)}/files`)
      .then((f) => {
        compose = f.compose;
        env = f.env;
        composeFile = f.composeFile;
        saved = { compose: f.compose, env: f.env };
        loadedFiles = true;
      })
      .catch((e) => toast(e.message, 'error'));
  });

  $effect(() => {
    if (!dirty) return;
    const warn = (e) => e.preventDefault();
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  });

  async function validate() {
    validation = { checking: true };
    try {
      validation = await api.post(`/stacks/${encodeURIComponent(name)}/validate`, { compose, env });
    } catch (e) {
      validation = { valid: false, error: e.message };
    }
    return validation.valid;
  }

  async function save(deploy = false) {
    saving = true;
    const ok = await validate();
    if (!ok) {
      saving = false;
      toast('Fix the problem in the compose file first.', 'error');
      return;
    }
    try {
      await api.put(`/stacks/${encodeURIComponent(name)}/files`, { compose, env });
      saved = { compose, env };
      toast('Saved', 'success', 2000);
      if (deploy) runJob(`Deploy ${name}`, 'POST', `/stacks/${encodeURIComponent(name)}/action/up`);
    } catch (e) {
      toast(e.message, 'error');
    }
    saving = false;
  }

  function goTab(id) {
    navigate(id === 'overview' ? `/stacks/${name}` : `/stacks/${name}/${id}`, { replace: true });
  }

  const moreItems = $derived([
    { label: 'Pull latest images', icon: 'download', hint: "Download only, don't restart", onclick: () => stackAction(name, 'pull') },
    stack?.total ? { label: 'Take down', icon: 'power', hint: 'Remove containers, keep data', onclick: () => stackAction(name, 'down') } : null,
    { divider: true },
    { label: 'Delete stack', icon: 'trash', danger: true, onclick: () => deleteStack(name) },
  ]);
</script>

<div class="page">
  <a class="back" href="/stacks"><Icon name="arrow-left" size={14} /> Stacks</a>

  {#if notFound}
    <div class="card empty">
      <h3>Stack not found</h3>
      <p>There's no stack called {name}. It may have been deleted.</p>
      <a class="btn" href="/stacks">Back to stacks</a>
    </div>
  {:else}
    <div class="page-header">
      <div class="title">
        <div class="row">
          <h1>{name}</h1>
          {#if stack}
            <span class="badge {stateColor(stack.status)}"><span class="dot {stateColor(stack.status)}" style="box-shadow:none"></span>{stackStatusLabel(stack.status)}</span>
            {#if stack.external}<span class="badge">External</span>{/if}
          {/if}
        </div>
        {#if stack?.dir}<p class="sub mono">{stack.dir}{stack.composeFile ? '/' + stack.composeFile : ''}</p>{/if}
      </div>

      {#if stack && !stack.external}
        <div class="row wrap">
          {#if stack.busy}
            <span class="badge accent"><Icon name="loader" size={12} class="spin" /> {stack.busy} in progress</span>
          {/if}
          {#if up}
            <button onclick={() => stackAction(name, 'restart')} disabled={!!stack.busy}><Icon name="restart" size={15} /> Restart</button>
            <button onclick={() => stackAction(name, 'stop')} disabled={!!stack.busy}><Icon name="stop" size={15} /> Stop</button>
          {:else}
            <button class="primary" onclick={() => stackAction(name, 'up')} disabled={!!stack.busy}><Icon name="play" size={15} /> {stack.status === 'inactive' ? 'Deploy' : 'Start'}</button>
          {/if}
          <button class:primary={stack.updateAvailable && up} onclick={() => stackAction(name, 'update')} disabled={!!stack.busy} title="Pull the latest images and recreate changed containers">
            <Icon name="download" size={15} /> Update{#if stack.updateAvailable}<span class="upd-dot"></span>{/if}
          </button>
          <Menu items={moreItems} />
        </div>
      {/if}
    </div>

    <nav class="tabs">
      {#each tabs as t}
        <a href={t.id === 'overview' ? `/stacks/${name}` : `/stacks/${name}/${t.id}`} class:active={tab === t.id}
          onclick={(e) => { e.preventDefault(); goTab(t.id); }}>
          <Icon name={t.icon} size={15} /> {t.label}{#if t.id === 'editor' && dirty}<span class="unsaved" title="Unsaved changes"></span>{/if}
        </a>
      {/each}
    </nav>

    {#if tab === 'overview'}
      {#if stack?.external}
        <div class="note"><Icon name="info" size={16} /> This project was started outside your stacks folder, so only its containers can be managed here.</div>
      {/if}
      {#if stack?.updateAvailable}
        <div class="note amber"><Icon name="upgrade" size={16} /> Newer images are available. Use <strong>Update</strong> to pull them and recreate the containers.</div>
      {/if}

      <div class="tiles">
        <div class="card tile"><span>CPU</span><strong>{up ? percent(totalCpu) : '–'}</strong></div>
        <div class="card tile"><span>Memory</span><strong>{up ? bytes(totalMem) : '–'}</strong></div>
        <div class="card tile"><span>Network I/O</span><strong>{up ? bytes(totalNet) : '–'}</strong></div>
        <div class="card tile"><span>Containers</span><strong>{stack ? `${stack.running} / ${stack.total}` : '–'}</strong></div>
      </div>

      <div class="card">
        {#if stack && !stack.containers.length}
          <div class="empty">
            <h3>Not deployed</h3>
            <p>This stack has no containers yet. Deploy it to start its services.</p>
            <button class="primary" onclick={() => stackAction(name, 'up')}><Icon name="play" size={15} /> Deploy</button>
          </div>
        {:else}
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr><th>Service</th><th class="hide-sm">Image</th><th>State</th><th>Ports</th><th class="num">CPU</th><th class="num hide-sm">Memory</th><th></th></tr>
              </thead>
              <tbody>
                {#each stack?.containers || [] as c (c.id)}
                  {@const st = stats[c.id]}
                  <tr class="clickable" onclick={() => navigate(`/containers/${c.id.slice(0, 12)}`)}>
                    <td>
                      <div class="row"><span class="dot {stateColor(c.state, c.health)}"></span>
                        <div class="cname"><strong>{c.service || c.name}</strong><span class="dim">{c.name}</span></div>
                      </div>
                    </td>
                    <td class="hide-sm"><span class="mono truncate img">{c.image}</span>{#if c.updateAvailable}<span class="badge amber tiny">Update</span>{/if}</td>
                    <td class="nowrap">{containerStateLabel(c.state)}{#if c.health}<span class="dim"> · {c.health}</span>{/if}</td>
                    <td>
                      <div class="ports">
                        {#if c.webUI}<a class="port web" href={appUrl(c.webUI)} target="_blank" rel="noopener noreferrer" title={appUrl(c.webUI)} onclick={(e) => e.stopPropagation()}><Icon name="globe" size={11} /> Open</a>{/if}
                        {#each c.ports.filter((p) => p.public) as p}
                          <a class="port" href={portUrl(p)} target="_blank" rel="noreferrer" onclick={(e) => e.stopPropagation()}>{p.public}:{p.private}</a>
                        {:else}{#if !c.webUI}<span class="dim">–</span>{/if}{/each}
                      </div>
                    </td>
                    <td class="num">{st ? percent(st.cpu) : '–'}</td>
                    <td class="num hide-sm">{st ? bytes(st.mem) : '–'}</td>
                    <td class="actions" onclick={(e) => e.stopPropagation()}>
                      <button class="ghost icon sm" title="Logs" aria-label="Logs" onclick={() => navigate(`/containers/${c.id.slice(0, 12)}/logs`)}><Icon name="logs" size={15} /></button>
                      <button class="ghost icon sm" title="Console" aria-label="Console" disabled={c.state !== 'running'} onclick={() => navigate(`/containers/${c.id.slice(0, 12)}/console`)}><Icon name="terminal" size={15} /></button>
                      <Menu size="sm" items={[
                        c.state === 'running'
                          ? { label: 'Restart', icon: 'restart', onclick: () => containerAction(c, 'restart') }
                          : { label: 'Start', icon: 'play', onclick: () => containerAction(c, 'start') },
                        c.state === 'running' ? { label: 'Stop', icon: 'stop', onclick: () => containerAction(c, 'stop') } : null,
                      ]} />
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
      </div>
    {:else if tab === 'logs'}
      <LogViewer path={`/stacks/${encodeURIComponent(name)}/logs`} />
    {:else if tab === 'editor'}
      <div class="editor-bar">
        <span class="mono dim">{composeFile}</span>
        {#if dirty}<span class="badge amber">Unsaved changes</span>{/if}
        <span class="spacer"></span>
        <button onclick={validate}><Icon name="check" size={15} /> Validate</button>
        <button onclick={() => save(false)} disabled={!dirty || saving}><Icon name="save" size={15} /> Save</button>
        <button class="primary" onclick={() => save(true)} disabled={saving}><Icon name="rocket" size={15} /> Save and deploy</button>
      </div>
      {#if validation && !validation.checking}
        <div class="note {validation.valid ? 'green' : 'red'}">
          <Icon name={validation.valid ? 'check-circle' : 'x-circle'} size={16} />
          {#if validation.valid}The compose file is valid.{:else}<pre class="verr">{validation.error}</pre>{/if}
        </div>
      {/if}
      {#if loadedFiles}
        <CodeEditor bind:value={compose} language="yaml" height="calc(100vh - 470px)" onsave={() => save(false)} />
        <div class="env-head">
          <h3>Environment variables</h3>
          <span class="dim">.env · used for <code>{'${VARIABLES}'}</code> in the compose file. Kept private on disk.</span>
        </div>
        <CodeEditor bind:value={env} language="env" height="150px" onsave={() => save(false)} placeholder="TZ=Europe/London" />
      {:else}
        <div class="skeleton" style="height:400px"></div>
      {/if}
    {/if}
  {/if}
</div>

<style>
  .back { display: inline-flex; align-items: center; gap: 5px; color: var(--text-2); font-size: 13px; margin-bottom: 12px; }
  .title h1 { font-size: 24px; }
  .title .row { gap: 10px; flex-wrap: wrap; }
  .badge.green, .badge.amber, .badge.red { gap: 6px; }
  .upd-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--amber); margin-left: 2px; }
  .unsaved { width: 7px; height: 7px; border-radius: 50%; background: var(--amber); }
  .tiles { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 16px; }
  .tile { padding: 12px 16px; display: grid; gap: 2px; }
  .tile span { font-size: 12.5px; color: var(--text-2); }
  .tile strong { font-size: 19px; font-weight: 600; font-variant-numeric: tabular-nums; }
  .cname { display: grid; line-height: 1.3; min-width: 0; }
  .cname span { font-size: 12px; }
  .img { display: inline-block; max-width: 240px; vertical-align: middle; }
  .tiny { height: 18px; font-size: 11px; margin-left: 6px; }
  .ports { display: flex; gap: 4px; flex-wrap: wrap; }
  .num { text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 14px; background: var(--accent-soft); color: var(--accent-text); font-size: 13.5px; }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note.green { background: var(--green-soft); color: var(--green-text); }
  .note.red { background: var(--red-soft); color: var(--red-text); }
  .verr { margin: 0; white-space: pre-wrap; font-size: 12.5px; }
  .editor-bar { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; flex-wrap: wrap; }
  .spacer { flex: 1; }
  .env-head { display: flex; align-items: baseline; gap: 10px; margin: 18px 0 8px; flex-wrap: wrap; }
  .env-head .dim { font-size: 12.5px; }
  @media (max-width: 860px) { .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
