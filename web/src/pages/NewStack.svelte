<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast, runJob } from '../lib/state.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import { nameColor } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import CodeEditor from '../components/CodeEditor.svelte';

  let templates = $state([]);
  let chosen = $state(null);
  let search = $state('');
  let cat = $state('All');
  let name = $state('');
  let compose = $state('');
  let env = $state('');
  let busy = $state(false);
  let error = $state('');
  let validation = $state(null);
  let advanced = $state(false);
  let shown = $state({}); // secrets being shown
  let usedPorts = $state({}); // host port -> container using it
  let stackNames = $state([]);

  onMount(async () => {
    try {
      // Filled in for this server; the time zone is the browser's.
      templates = await api.get(`/templates?tz=${encodeURIComponent(Intl.DateTimeFormat().resolvedOptions().timeZone || '')}`);
    } catch (e) {
      toast(e.message, 'error');
    }
  });

  const catIcon = {
    Start: 'file', Media: 'film', 'Media management': 'layers', Downloads: 'download', 'Photos and books': 'image',
    'Media tools': 'wrench', 'Home automation': 'home', Network: 'globe', Monitoring: 'activity', Productivity: 'briefcase',
    Security: 'shield', 'Files and backup': 'archive', Development: 'git', AI: 'sparkles', Games: 'gamepad',
  };
  const categories = $derived([...new Set(templates.map((t) => t.category))]);
  const counts = $derived(Object.fromEntries(categories.map((c) => [c, templates.filter((t) => t.category === c).length])));
  const matches = (t) => !search || `${t.name} ${t.description} ${t.category}`.toLowerCase().includes(search.toLowerCase());
  const filtered = $derived(templates.filter((t) => matches(t) && (cat === 'All' || search || t.category === cat)));
  const grouped = $derived(cat === 'All' && !search);
  const initials = (n) => n.replace(/[^A-Za-z0-9 ]/g, '').split(/\s+/).filter(Boolean).slice(0, 2).map((w) => w[0]).join('').toUpperCase() || '?';

  // ---------- Install ----------

  // Fresh random secrets for placeholder passwords, so nobody runs with "change-me".
  function secret(n = 24) {
    const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789';
    const limit = 256 - (256 % chars.length); // no bias towards the first letters
    let out = '';
    while (out.length < n) {
      for (const x of crypto.getRandomValues(new Uint8Array(n))) if (x < limit && out.length < n) out += chars[x % chars.length];
    }
    return out;
  }
  const isSecret = (k) => /PASSWORD|SECRET|TOKEN|AUTHKEY|(^|_)KEY$/.test(k);
  function fillSecrets(text) {
    return text.replace(/^([A-Z0-9_]+)=change-me.*$/gm, (line, k) => (isSecret(k) ? `${k}=${secret()}` : line));
  }

  async function pick(t) {
    chosen = t;
    compose = t.compose;
    env = fillSecrets(t.env || '');
    name = t.id === 'blank' ? '' : t.id;
    validation = null;
    error = '';
    advanced = t.id === 'blank' || !t.env;
    shown = {};
    window.scrollTo({ top: 0 });
    try {
      const [cs, ss] = await Promise.all([api.get('/containers'), api.get('/stacks')]);
      const used = {};
      for (const c of cs) for (const p of c.ports || []) if (p.public) used[`${p.public}/${p.type}`] = c.name;
      usedPorts = used;
      stackNames = ss.map((s) => s.name);
    } catch {}
  }

  // The .env as form fields: each variable with the comment above it as its hint.
  const vars = $derived.by(() => {
    const out = [];
    let hint = '';
    for (const line of env.split('\n')) {
      if (line.startsWith('#')) {
        hint = line.replace(/^#\s?/, '');
        continue;
      }
      const m = line.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/);
      if (m) out.push({ key: m[1], value: m[2], hint });
      hint = '';
    }
    return out;
  });
  const envMap = $derived(Object.fromEntries(vars.map((x) => [x.key, x.value])));
  function setVar(key, value) {
    env = env.replace(new RegExp(`^${key}=.*$`, 'm'), () => `${key}=${value.replace(/\n/g, ' ')}`);
  }
  const kindOf = (k, v) => (isSecret(k) ? 'secret' : /_PORT$/.test(k) ? 'port' : /(_PATH|_LOCATION|_DIR)$/.test(k) || /^\.?\//.test(v) ? 'path' : 'text');

  // Host ports the compose file publishes, with the .env filled in.
  const ports = $derived.by(() => {
    const out = [];
    const re = /^\s*-\s*["']?(?:[\d.]+:)?(\$\{(\w+)(?::?-([^}]*))?\}|\d+):(?:\$\{\w+\}|\d+)(?:\/(tcp|udp))?["']?\s*(?:#.*)?$/gm;
    for (const m of compose.matchAll(re)) {
      const port = m[2] ? (envMap[m[2]] ?? m[3]) : m[1];
      if (port) out.push({ port: +port, proto: m[4] || 'tcp', key: m[2] || '' });
    }
    if (/network_mode:\s*["']?host/.test(compose) && chosen?.port) out.push({ port: +chosen.port, proto: 'tcp', key: '', host: true });
    return out;
  });
  const conflicts = $derived(ports.filter((p) => usedPorts[`${p.port}/${p.proto}`]).map((p) => ({ ...p, by: usedPorts[`${p.port}/${p.proto}`] })));
  const conflictFor = (key) => conflicts.find((c) => c.key === key);

  const nameOk = $derived(/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name));
  const nameTaken = $derived(stackNames.includes(name));

  async function create(deploy) {
    error = '';
    if (!nameOk) {
      error = 'Use lowercase letters, numbers, dashes and underscores for the name.';
      return;
    }
    if (nameTaken) {
      error = `There's already a stack called ${name}.`;
      return;
    }
    busy = true;
    try {
      validation = await api.post(`/stacks/${encodeURIComponent(name)}/validate`, { compose, env });
      if (!validation.valid) {
        advanced = true;
        busy = false;
        return;
      }
      await api.post('/stacks', { name, compose, env });
      toast(`Created ${name}`, 'success');
      navigate(`/stacks/${name}`);
      if (deploy) runJob(`Deploy ${name}`, 'POST', `/stacks/${encodeURIComponent(name)}/action/up`);
    } catch (e) {
      error = e.message;
    }
    busy = false;
  }
</script>

<div class="page">
  <a class="back" href="/stacks"><Icon name="arrow-left" size={14} /> Stacks</a>

  {#if !chosen}
    <div class="page-header">
      <div>
        <h1>Apps</h1>
        <p class="sub">{templates.length ? `${templates.length - 1} ready-made apps.` : ''} Pick one to fill in its settings, or start from a blank compose file. You can review everything before it runs.</p>
      </div>
      <div class="search tsearch">
        <Icon name="search" size={15} />
        <input placeholder="Search apps" bind:value={search} />
      </div>
    </div>

    <div class="catalog">
      <nav class="cats" aria-label="Categories">
        <button class:active={cat === 'All' && !search} onclick={() => ((cat = 'All'), (search = ''))}><Icon name="dashboard" size={15} /><span>All apps</span><small>{templates.length}</small></button>
        {#each categories as c}
          <button class:active={cat === c && !search} onclick={() => ((cat = c), (search = ''))}><Icon name={catIcon[c] || 'box'} size={15} /><span>{c === 'Start' ? 'Start from scratch' : c}</span><small>{counts[c]}</small></button>
        {/each}
      </nav>

      <div class="apps">
        {#snippet card(t)}
          <button class="card tcard" onclick={() => pick(t)}>
            <div class="mono-tile" style="--c:{nameColor(t.name)}">{t.id === 'blank' ? '+' : initials(t.name)}</div>
            <div class="tbody">
              <div class="row"><strong class="truncate">{t.name}</strong>{#if t.port}<span class="port">:{t.port}</span>{/if}</div>
              <span class="muted desc">{t.description}</span>
              {#if !grouped && cat === 'All'}<span class="dim tcat"><Icon name={catIcon[t.category] || 'box'} size={11} /> {t.category}</span>{/if}
            </div>
          </button>
        {/snippet}
        {#if grouped}
          {#each categories as c}
            <h2 class="cat"><Icon name={catIcon[c] || 'box'} size={14} /> {c === 'Start' ? 'Start from scratch' : c}</h2>
            <div class="tgrid">{#each templates.filter((t) => t.category === c) as t (t.id)}{@render card(t)}{/each}</div>
          {/each}
        {:else}
          {#if search}<h2 class="cat">{filtered.length} result{filtered.length === 1 ? '' : 's'} for “{search}”</h2>{:else}<h2 class="cat"><Icon name={catIcon[cat] || 'box'} size={14} /> {cat}</h2>{/if}
          <div class="tgrid">{#each filtered as t (t.id)}{@render card(t)}{/each}</div>
          {#if templates.length && !filtered.length}
            <div class="card empty"><p>No apps match. <button class="linkish" onclick={() => pick(templates[0])}>Start with a blank file</button>.</p></div>
          {/if}
        {/if}
      </div>
    </div>
  {:else}
    <div class="page-header">
      <div class="row head">
        {#if chosen.id !== 'blank'}<div class="mono-tile big" style="--c:{nameColor(chosen.name)}">{initials(chosen.name)}</div>{/if}
        <div>
          <h1>{chosen.id === 'blank' ? 'New stack' : chosen.name}</h1>
          <p class="sub">{chosen.description}{#if chosen.project}{' '}<a href={chosen.project} target="_blank" rel="noopener noreferrer">Project page <Icon name="external" size={11} /></a>{/if}</p>
        </div>
      </div>
      <button onclick={() => (chosen = null)}><Icon name="arrow-left" size={15} /> Choose another</button>
    </div>

    {#if chosen.notes}<div class="note info"><Icon name="info" size={16} /><span>{chosen.notes}</span></div>{/if}

    <div class="install">
      <section class="card">
        <div class="card-header"><h2>Stack</h2></div>
        <div class="card-body">
          <label class="field">
            <span>Name</span>
            <input class="stackname" bind:value={name} placeholder="jellyfin" autocapitalize="off" spellcheck="false" />
            {#if nameTaken}<span class="warn-text">There's already a stack called {name}.</span>{:else}<span class="hint">The folder name and the prefix for its containers. Lowercase letters, numbers, dashes and underscores.</span>{/if}
          </label>
        </div>
      </section>

      {#if vars.length}
        <section class="card">
          <div class="card-header"><h2>Settings</h2><span class="dim small">Saved in the stack's .env file</span></div>
          <div class="card-body vars">
            {#each vars as x (x.key)}
              {@const kind = kindOf(x.key, x.value)}
              {@const clash = kind === 'port' ? conflictFor(x.key) : null}
              <label class="field">
                <span class="vlabel">{x.hint || x.key}{#if kind === 'path'}<span class="badge tiny">folder</span>{/if}</span>
                {#if kind === 'secret'}
                  <div class="row">
                    <input class="mono" type={shown[x.key] ? 'text' : 'password'} value={x.value} oninput={(e) => setVar(x.key, e.target.value)} autocomplete="off" spellcheck="false" />
                    <button type="button" class="ghost icon sm" aria-label={shown[x.key] ? 'Hide' : 'Show'} onclick={() => (shown[x.key] = !shown[x.key])}><Icon name={shown[x.key] ? 'eye-off' : 'eye'} size={15} /></button>
                    <button type="button" class="ghost icon sm" title="New random value" aria-label="New random value" onclick={() => setVar(x.key, secret())}><Icon name="refresh" size={14} /></button>
                  </div>
                {:else}
                  <input class:mono={kind !== 'text'} type={kind === 'port' ? 'number' : 'text'} min="1" max="65535" value={x.value} oninput={(e) => setVar(x.key, e.target.value)} spellcheck="false" autocapitalize="off" />
                {/if}
                <span class="key mono">{x.key}{#if clash}<span class="warn-text"> · already used by {clash.by}</span>{/if}</span>
              </label>
            {/each}
          </div>
        </section>
      {/if}

      {#if conflicts.some((c) => !c.key)}
        <div class="note warn"><Icon name="alert" size={16} /><span>{conflicts.filter((c) => !c.key).map((c) => `Port ${c.port}${c.proto === 'udp' ? '/udp' : ''} is already used by ${c.by}`).join('. ')}. Change it in the compose file below{conflicts.some((c) => c.host) ? ' (this app uses the host network, so it needs the port free)' : ''}.</span></div>
      {/if}

      <section class="card">
        <button class="card-header toggle-head" onclick={() => (advanced = !advanced)} aria-expanded={advanced}>
          <Icon name={advanced ? 'chevron-down' : 'chevron-right'} size={15} /><h2>Compose file and .env</h2><span class="dim small">{advanced ? '' : 'Advanced: edit everything by hand'}</span>
        </button>
        {#if advanced}
          <div class="card-body">
            <h3 class="lbl">compose.yaml</h3>
            <CodeEditor bind:value={compose} language="yaml" height="440px" />
            <h3 class="lbl">Environment variables <span class="dim">(.env)</span></h3>
            <CodeEditor bind:value={env} language="env" height="200px" placeholder="TZ=Europe/London" />
          </div>
        {/if}
      </section>
    </div>

    {#if validation && !validation.valid}
      <div class="note red"><Icon name="x-circle" size={16} /><pre>{validation.error}</pre></div>
    {/if}
    {#if error}<p class="error-text">{error}</p>{/if}

    <div class="row actions">
      <span class="spacer"></span>
      <button onclick={() => create(false)} disabled={busy}>Create</button>
      <button class="primary" onclick={() => create(true)} disabled={busy}>
        {#if busy}<Icon name="loader" class="spin" size={15} />{:else}<Icon name="rocket" size={15} />{/if} Create and deploy
      </button>
    </div>
  {/if}
</div>

<style>
  .back { display: inline-flex; align-items: center; gap: 5px; color: var(--text-2); font-size: 13px; margin-bottom: 12px; }
  .tsearch { width: 280px; max-width: 100%; }
  .catalog { display: grid; grid-template-columns: 210px minmax(0, 1fr); gap: 24px; align-items: start; }
  .cats { display: grid; gap: 2px; position: sticky; top: 16px; }
  .cats button { all: unset; box-sizing: border-box; display: flex; align-items: center; gap: 9px; padding: 7px 10px; border-radius: 8px; font-size: 13.5px; color: var(--text-2); cursor: pointer; }
  .cats button span { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cats button small { font-size: 12px; color: var(--text-3); font-variant-numeric: tabular-nums; }
  .cats button:hover { background: var(--surface-2); color: var(--text); }
  .cats button.active { background: var(--accent-soft); color: var(--accent-text); }
  .cats button.active small { color: var(--accent-text); }
  .cats button:focus-visible { outline: 2px solid var(--accent); }
  .cat { display: flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); font-weight: 600; margin: 4px 0 10px; }
  .apps .tgrid + .cat { margin-top: 24px; }
  .tgrid { display: grid; grid-template-columns: repeat(auto-fill, minmax(250px, 1fr)); gap: 12px; }
  .tcard { all: unset; box-sizing: border-box; display: flex; gap: 13px; padding: 14px; cursor: pointer; align-items: flex-start; background: var(--surface); border: 1px solid var(--border); border-radius: 12px; transition: border-color 0.12s, box-shadow 0.12s, transform 0.12s; min-width: 0; }
  .tcard:hover { border-color: color-mix(in srgb, var(--accent) 45%, var(--border)); box-shadow: var(--shadow-lg); transform: translateY(-1px); }
  .tcard:focus-visible { outline: 2px solid var(--accent); }
  .mono-tile { width: 40px; height: 40px; border-radius: 10px; flex: none; display: grid; place-items: center; font-weight: 700; font-size: 14px; letter-spacing: -0.02em; color: color-mix(in srgb, var(--c) 70%, var(--text)); background: color-mix(in srgb, var(--c) 16%, var(--surface)); border: 1px solid color-mix(in srgb, var(--c) 30%, transparent); }
  .mono-tile.big { width: 52px; height: 52px; font-size: 18px; border-radius: 13px; }
  .tbody { display: grid; gap: 3px; min-width: 0; }
  .tbody strong { font-size: 14px; }
  .desc { font-size: 13px; line-height: 1.45; display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; }
  .tcat { display: inline-flex; align-items: center; gap: 4px; font-size: 11.5px; margin-top: 2px; }
  .head { gap: 14px; align-items: center; }
  .install { display: grid; gap: 14px; }
  .stackname { max-width: 420px; }
  .vars { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 16px 20px; }
  .field { display: grid; gap: 5px; align-content: start; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .vlabel { display: flex; align-items: center; gap: 6px; }
  .key { font-size: 11.5px; color: var(--text-3); }
  .warn-text { color: var(--amber-text); font-size: 12.5px; font-family: var(--font); }
  .tiny { height: 18px; font-size: 10.5px; padding: 0 6px; }
  .small { font-size: 12.5px; }
  .toggle-head { all: unset; box-sizing: border-box; width: 100%; display: flex; align-items: center; gap: 8px; padding: 14px 18px; cursor: pointer; }
  .toggle-head:focus-visible { outline: 2px solid var(--accent); border-radius: 12px; }
  .lbl { margin: 4px 0 8px; font-size: 13.5px; }
  .lbl + :global(.editor) { margin-bottom: 14px; }
  .note { display: flex; gap: 10px; padding: 11px 14px; border-radius: 10px; margin-bottom: 14px; font-size: 13.5px; align-items: flex-start; }
  .note :global(svg) { flex: none; margin-top: 1px; }
  .note.info { background: var(--accent-soft); color: var(--accent-text); }
  .note.warn { background: var(--amber-soft); color: var(--amber-text); margin: 0; }
  .note.red { background: var(--red-soft); color: var(--red-text); margin-top: 14px; }
  .note pre { margin: 0; white-space: pre-wrap; }
  .actions { margin-top: 18px; }
  .spacer { flex: 1; }
  .linkish { all: unset; color: var(--accent-text); cursor: pointer; }
  @media (max-width: 860px) {
    .catalog { grid-template-columns: 1fr; }
    .cats { position: static; display: flex; overflow-x: auto; padding-bottom: 4px; }
    .cats button { flex: none; }
    .cats button span { overflow: visible; }
  }
</style>
