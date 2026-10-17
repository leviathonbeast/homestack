<script>
  import { api, subscribe } from '../lib/api.js';
  import { live, toast } from '../lib/state.svelte.js';
  import { bytes, percent, stateColor, containerStateLabel, dateTime, ago, shortId, portUrl, appUrl } from '../lib/format.js';
  import { containerAction, removeContainer } from '../lib/actions.js';
  import { navigate } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import { copy } from '../lib/clipboard.js';
  import Menu from '../components/Menu.svelte';
  import LogViewer from '../components/LogViewer.svelte';
  import Terminal from '../components/Terminal.svelte';

  let { id, tab = 'overview' } = $props();

  let data = $state(null);
  let notFound = $state(false);
  let stats = $state(null);
  let showEnv = $state(false);
  let webEdit = $state(null); // null = not editing, else the address being typed

  async function saveWeb(url) {
    try {
      await api.put('/apps/container', { container: c.name, name: c.service || c.name, url });
      toast(url ? 'Web address saved' : 'Web address removed', 'success', 1800);
      webEdit = null;
      data = await api.get(`/containers/${encodeURIComponent(id)}`);
    } catch (e) {
      toast(e.message, 'error', 6000);
    }
  }

  $effect(() => {
    live.tick;
    api
      .get(`/containers/${encodeURIComponent(id)}`)
      .then((d) => (data = d))
      .catch((e) => (e.status === 404 ? (notFound = true) : toast(e.message, 'error')));
  });

  const ins = $derived(data?.inspect);
  const c = $derived(
    data?.summary || (ins && { id: ins.Id, name: ins.Name?.replace(/^\//, ''), state: ins.State?.Status, ports: [], image: ins.Config?.Image }),
  );
  const running = $derived(c?.state === 'running');

  $effect(() => {
    if (tab !== 'overview' || !running) return;
    return subscribe(`/containers/${encodeURIComponent(id)}/stats`, { stats: (s) => (stats = Object.values(s)[0] || null) });
  });

  const env = $derived((ins?.Config?.Env || []).map((e) => {
    const i = e.indexOf('=');
    return [e.slice(0, i), e.slice(i + 1)];
  }));
  const labels = $derived(Object.entries(ins?.Config?.Labels || {}).sort());
  const networks = $derived(Object.entries(ins?.NetworkSettings?.Networks || {}));
  const secretish = /pass|secret|token|key|pwd|auth/i;

  const tabs = [
    { id: 'overview', label: 'Overview', icon: 'dashboard' },
    { id: 'logs', label: 'Logs', icon: 'logs' },
    { id: 'console', label: 'Console', icon: 'terminal' },
    { id: 'inspect', label: 'Inspect', icon: 'inspect' },
  ];
  function goTab(t) {
    navigate(t === 'overview' ? `/containers/${id}` : `/containers/${id}/${t}`, { replace: true });
  }

  // Console
  let shell = $state('auto');
  let customShell = $state('');
  let user = $state('');
  let session = $state(0);
  let termStatus = $state('');

  async function remove() {
    if (await removeContainer(c)) navigate('/containers');
  }
  const copyInspect = () => copy(JSON.stringify(ins, null, 2));
</script>

<div class="page">
  <a class="back" href={c?.stack ? `/stacks/${c.stack}` : '/containers'}><Icon name="arrow-left" size={14} /> {c?.stack || 'Containers'}</a>

  {#if notFound}
    <div class="card empty">
      <h3>Container not found</h3>
      <p>It may have been removed or recreated by a redeploy.</p>
      <a class="btn" href="/containers">Back to containers</a>
    </div>
  {:else}
    <div class="page-header">
      <div>
        <div class="row wrap" style="gap:10px">
          <h1>{c?.name || id}</h1>
          {#if c}<span class="badge {stateColor(c.state, c.health)}">{containerStateLabel(c.state)}{c.health ? ` · ${c.health}` : ''}</span>{/if}
          {#if c?.updateAvailable}<span class="badge amber">Update available</span>{/if}
        </div>
        {#if ins}<p class="sub"><span class="mono">{ins.Config.Image}</span> · {shortId(ins.Id)}</p>{/if}
      </div>
      {#if c}
        <div class="row wrap">
          {#if running}
            <button onclick={() => containerAction(c, 'restart')}><Icon name="restart" size={15} /> Restart</button>
            <button onclick={() => containerAction(c, 'stop')}><Icon name="stop" size={15} /> Stop</button>
          {:else if c.state === 'paused'}
            <button class="primary" onclick={() => containerAction(c, 'unpause')}><Icon name="play" size={15} /> Resume</button>
          {:else}
            <button class="primary" onclick={() => containerAction(c, 'start')}><Icon name="play" size={15} /> Start</button>
          {/if}
          <Menu items={[
            running ? { label: 'Pause', icon: 'pause', onclick: () => containerAction(c, 'pause') } : null,
            running ? { label: 'Kill', icon: 'power', hint: 'Stop immediately', onclick: () => containerAction(c, 'kill') } : null,
            running ? { divider: true } : null,
            { label: 'Remove', icon: 'trash', danger: true, onclick: remove },
          ]} />
        </div>
      {/if}
    </div>

    <nav class="tabs">
      {#each tabs as t}
        <a href={t.id === 'overview' ? `/containers/${id}` : `/containers/${id}/${t.id}`} class:active={tab === t.id}
          onclick={(e) => { e.preventDefault(); goTab(t.id); }}><Icon name={t.icon} size={15} /> {t.label}</a>
      {/each}
    </nav>

    {#if !ins}
      <div class="skeleton" style="height:300px"></div>
    {:else if tab === 'overview'}
      <div class="tiles">
        <div class="card tile"><span>CPU</span><strong>{running && stats ? percent(stats.cpu) : '–'}</strong></div>
        <div class="card tile">
          <span>Memory</span><strong>{running && stats ? bytes(stats.mem) : '–'}</strong>
          {#if running && stats}<div class="meter"><div style="width:{Math.min(100, (stats.mem / stats.memLimit) * 100)}%"></div></div>{/if}
        </div>
        <div class="card tile"><span>Network in / out</span><strong>{running && stats ? `${bytes(stats.netRx)} / ${bytes(stats.netTx)}` : '–'}</strong></div>
        <div class="card tile"><span>Disk read / write</span><strong>{running && stats ? `${bytes(stats.blkRead)} / ${bytes(stats.blkWrite)}` : '–'}</strong></div>
      </div>

      <div class="cols">
        <section class="card">
          <div class="card-header"><h2>Details</h2></div>
          <dl>
            <dt>Image</dt><dd class="mono">{ins.Config.Image}</dd>
            <dt>Created</dt><dd>{dateTime(ins.Created)} <span class="dim">({ago(ins.Created)})</span></dd>
            {#if running}<dt>Started</dt><dd>{dateTime(ins.State.StartedAt)} <span class="dim">({ago(ins.State.StartedAt)})</span></dd>{/if}
            {#if !running && ins.State.FinishedAt && !ins.State.FinishedAt.startsWith('0001')}<dt>Stopped</dt><dd>{dateTime(ins.State.FinishedAt)} · exit code {ins.State.ExitCode}</dd>{/if}
            <dt>Restart policy</dt><dd>{ins.HostConfig.RestartPolicy?.Name || 'no'}{ins.RestartCount ? ` · restarted ${ins.RestartCount} times` : ''}</dd>
            <dt>Command</dt><dd class="mono">{[...(ins.Config.Entrypoint || []), ...(ins.Config.Cmd || [])].join(' ') || '–'}</dd>
            {#if ins.Config.WorkingDir}<dt>Working folder</dt><dd class="mono">{ins.Config.WorkingDir}</dd>{/if}
            {#if ins.Config.User}<dt>User</dt><dd class="mono">{ins.Config.User}</dd>{/if}
            {#if c?.stack}<dt>Stack</dt><dd><a href="/stacks/{c.stack}">{c.stack}</a> · service {c.service}</dd>{/if}
          </dl>
        </section>

        <div class="stack-v">
          <section class="card">
            <div class="card-header"><h2>Web page</h2>
              <span class="spacer"></span>
              {#if webEdit === null}<button class="sm ghost" onclick={() => (webEdit = c?.webUI || 'http://[HOST]:')}>{c?.webUI ? 'Change' : 'Set address'}</button>{/if}
            </div>
            <div class="card-body stack-v">
              {#if webEdit !== null}
                <form class="row" onsubmit={(e) => { e.preventDefault(); saveWeb(webEdit.trim()); }}>
                  <input class="mono" bind:value={webEdit} placeholder="http://[HOST]:8080/" spellcheck="false" aria-label="Web address" />
                  <button class="primary" type="submit" disabled={!webEdit.trim()}>Save</button>
                  <button type="button" class="ghost" onclick={() => (webEdit = null)}>Cancel</button>
                </form>
                <span class="hint"><code>[HOST]</code> is replaced with the address you opened Homestack with. The link also appears on the dashboard.</span>
                {#if c?.webUISaved}<div><button class="sm danger" type="button" onclick={() => saveWeb('')}>Remove the saved address</button></div>{/if}
              {:else if c?.webUI}
                <div class="row">
                  <a class="btn sm" href={appUrl(c.webUI)} target="_blank" rel="noopener noreferrer"><Icon name="globe" size={14} /> Open</a>
                  <span class="mono dim truncate">{appUrl(c.webUI)}</span>
                </div>
                {#if !c.webUISaved}<span class="hint">Worked out from its ports. Set it yourself if it's wrong.</span>{/if}
              {:else}
                <span class="dim small">No web page found. If this app has one, set its address.</span>
              {/if}
            </div>
          </section>
          <section class="card">
            <div class="card-header"><h2>Ports</h2></div>
            <div class="list">
              {#each c?.ports || [] as p}
                <div class="li">
                  {#if p.public}<a class="port" href={portUrl(p)} target="_blank" rel="noreferrer">{p.public} <Icon name="external" size={11} /></a> <span class="dim">→</span>{/if}
                  <span class="mono">{p.private}/{p.type}</span>
                  {#if !p.public}<span class="dim small">not published</span>{/if}
                </div>
              {:else}<div class="li dim">No ports</div>{/each}
            </div>
          </section>
          <section class="card">
            <div class="card-header"><h2>Networks</h2></div>
            <div class="list">
              {#each networks as [n, cfg]}
                <div class="li"><strong>{n}</strong><span class="mono dim">{cfg.IPAddress || ''}</span></div>
              {:else}<div class="li dim">None</div>{/each}
            </div>
          </section>
        </div>
      </div>

      <section class="card sect">
        <div class="card-header"><h2>Storage</h2></div>
        <div class="list">
          {#each ins.Mounts || [] as m}
            <div class="li mount">
              <span class="badge">{m.Type}</span>
              <span class="mono truncate" title={m.Source}>{m.Type === 'volume' ? m.Name : m.Source}</span>
              <span class="dim">→</span>
              <span class="mono">{m.Destination}</span>
              {#if !m.RW}<span class="badge">read-only</span>{/if}
            </div>
          {:else}<div class="li dim">No volumes or bind mounts. Data inside this container is lost when it's recreated.</div>{/each}
        </div>
      </section>

      <section class="card sect">
        <div class="card-header">
          <h2>Environment</h2><span class="dim small">{env.length} variables</span><span class="spacer"></span>
          <button class="sm" onclick={() => (showEnv = !showEnv)}><Icon name={showEnv ? 'eye-off' : 'eye'} size={14} /> {showEnv ? 'Hide values' : 'Show values'}</button>
        </div>
        <div class="kv">
          {#each env as [k, v]}
            <span class="mono k">{k}</span>
            <span class="mono v">{showEnv || !secretish.test(k) ? v : '••••••••'}</span>
          {/each}
        </div>
      </section>

      {#if labels.length}
        <details class="card sect">
          <summary class="card-header"><h2>Labels</h2><span class="dim small">{labels.length}</span></summary>
          <div class="kv">
            {#each labels as [k, v]}<span class="mono k">{k}</span><span class="mono v">{v}</span>{/each}
          </div>
        </details>
      {/if}
    {:else if tab === 'logs'}
      <LogViewer path={`/containers/${encodeURIComponent(id)}/logs`} />
    {:else if tab === 'console'}
      {#if !running}
        <div class="card empty"><h3>The container isn't running</h3><p>Start it to open a console.</p>
          <button class="primary" onclick={() => containerAction(c, 'start')}><Icon name="play" size={15} /> Start</button></div>
      {:else}
        <div class="console-bar">
          <label class="mini"><span>Shell</span>
            <select bind:value={shell}>
              <option value="auto">Automatic (bash, then sh)</option>
              <option value="/bin/bash">bash</option>
              <option value="/bin/sh">sh</option>
              <option value="/bin/ash">ash</option>
              <option value="custom">Custom command</option>
            </select>
          </label>
          {#if shell === 'custom'}
            <label class="mini grow"><span>Command</span><input bind:value={customShell} placeholder="/usr/bin/fish" /></label>
          {/if}
          <label class="mini"><span>User</span><input bind:value={user} placeholder="default" style="width:120px" /></label>
          <button class="primary" onclick={() => session++}>
            <Icon name="terminal" size={15} /> {session ? 'Reconnect' : 'Connect'}
          </button>
          {#if session && termStatus}<span class="row small dim"><span class="dot" class:green={termStatus === 'connected'}></span>{termStatus}</span>{/if}
        </div>
        {#if session}
          {#key session}
            <div class="term-box">
              <Terminal containerId={id} shell={shell === 'custom' ? customShell : shell} {user} onstatus={(s) => (termStatus = s)} />
            </div>
          {/key}
        {:else}
          <div class="card empty">
            <h3>Open a shell inside {c.name}</h3>
            <p>Commands run inside the container, like <code>docker exec -it</code>.</p>
            <button class="primary" onclick={() => session++}><Icon name="terminal" size={15} /> Connect</button>
          </div>
        {/if}
      {/if}
    {:else if tab === 'inspect'}
      <div class="row" style="margin-bottom:10px"><span class="dim small">Raw output of docker inspect</span><span class="spacer"></span>
        <button class="sm" onclick={copyInspect}><Icon name="copy" size={14} /> Copy</button></div>
      <pre class="json">{JSON.stringify(ins, null, 2)}</pre>
    {/if}
  {/if}
</div>

<style>
  .back { display: inline-flex; align-items: center; gap: 5px; color: var(--text-2); font-size: 13px; margin-bottom: 12px; }
  h1 { font-size: 24px; word-break: break-all; }
  .tiles { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 16px; }
  .tile { padding: 12px 16px; display: grid; gap: 4px; }
  .tile span { font-size: 12.5px; color: var(--text-2); }
  .tile strong { font-size: 17px; font-weight: 600; font-variant-numeric: tabular-nums; }
  .cols { display: grid; grid-template-columns: minmax(0, 1.5fr) minmax(0, 1fr); gap: 16px; margin-bottom: 16px; }
  dl { display: grid; grid-template-columns: 140px 1fr; gap: 10px 16px; padding: 16px 18px; margin: 0; }
  dt { color: var(--text-2); font-size: 13px; }
  dd { margin: 0; overflow-wrap: anywhere; font-size: 13.5px; }
  .list { display: grid; }
  .li { display: flex; align-items: center; gap: 8px; padding: 10px 18px; border-bottom: 1px solid var(--border); font-size: 13.5px; min-width: 0; }
  .li:last-child { border-bottom: 0; }
  .li strong { flex: 1; }
  .sect { margin-bottom: 16px; }
  .kv { display: grid; grid-template-columns: minmax(140px, max-content) 1fr; gap: 6px 18px; padding: 14px 18px; max-height: 420px; overflow: auto; }
  .kv .k { color: var(--text-2); }
  .kv .v { overflow-wrap: anywhere; }
  details summary { cursor: pointer; list-style: none; }
  details:not([open]) summary { border-bottom: 0; }
  .small { font-size: 12.5px; }
  .spacer { flex: 1; }
  .console-bar { display: flex; align-items: flex-end; gap: 10px; margin-bottom: 12px; flex-wrap: wrap; }
  .mini { display: grid; gap: 4px; font-size: 12px; color: var(--text-2); }
  .mini select { width: auto; }
  .grow { flex: 1; min-width: 180px; }
  .term-box { height: calc(100vh - 330px); min-height: 360px; }
  .json { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 16px; overflow: auto; max-height: calc(100vh - 300px); font-size: 12px; line-height: 1.5; margin: 0; }
  @media (max-width: 1000px) { .cols { grid-template-columns: 1fr; } .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } dl { grid-template-columns: 110px 1fr; } }
</style>
