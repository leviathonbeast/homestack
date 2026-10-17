<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { live, toast, confirm, bumpLive } from '../lib/state.svelte.js';
  import { stateColor, containerStateLabel, portUrl, ago, appUrl, bytes, percent } from '../lib/format.js';
  import { containerAction, removeContainer } from '../lib/actions.js';
  import { navigate } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';
  import Modal from '../components/Modal.svelte';

  let list = $state(null);
  let error = $state('');
  let search = $state('');
  let filter = $state('all');

  $effect(() => {
    live.tick;
    api.get('/containers').then((l) => ((list = l), (error = ''))).catch((e) => (error = e.message));
  });

  const visible = (c) => {
    const q = search.toLowerCase();
    if (q && !(c.name + ' ' + c.image + ' ' + (c.stack || '')).toLowerCase().includes(q)) return false;
    if (filter === 'running') return c.state === 'running';
    if (filter === 'stopped') return c.state !== 'running';
    if (filter === 'standalone') return !c.stack;
    return true;
  };
  const filtering = $derived(!!search || filter !== 'all');

  function items(c) {
    const running = c.state === 'running';
    return [
      { label: 'Logs', icon: 'logs', onclick: () => navigate(`/containers/${c.id.slice(0, 12)}/logs`) },
      running ? { label: 'Console', icon: 'terminal', onclick: () => navigate(`/containers/${c.id.slice(0, 12)}/console`) } : null,
      c.stack ? { label: `Open ${c.stack} stack`, icon: 'layers', onclick: () => navigate(`/stacks/${c.stack}`) } : null,
      { label: 'Move to folder…', icon: 'folder', onclick: () => (moving = c) },
      { divider: true },
      running ? { label: 'Restart', icon: 'restart', onclick: () => containerAction(c, 'restart') } : null,
      running ? { label: 'Pause', icon: 'pause', onclick: () => containerAction(c, 'pause') } : null,
      c.state === 'paused' ? { label: 'Resume', icon: 'play', onclick: () => containerAction(c, 'unpause') } : null,
      running ? { label: 'Kill', icon: 'power', onclick: () => containerAction(c, 'kill') } : null,
      { divider: true },
      { label: 'Remove', icon: 'trash', danger: true, onclick: () => removeContainer(c) },
    ];
  }

  // ---------- Folders (like Unraid's FolderView) ----------

  let folders = $state([]);
  let editing = $state(null); // folder in the editor
  let moving = $state(null); // container being moved
  let dropOn = $state(null); // folder id (or '') under a dragged row
  let busyFolder = $state('');

  onMount(() => {
    api.get('/containers/folders').then((f) => (folders = f)).catch(() => {});
  });
  async function saveFolders(next) {
    const before = folders;
    folders = next;
    try {
      folders = await api.put('/containers/folders', { folders: next });
      return true;
    } catch (e) {
      folders = before;
      toast(e.message, 'error', 7000);
      return false;
    }
  }

  const patternOf = (f) => {
    try {
      return f.match ? new RegExp(f.match) : null;
    } catch {
      return null;
    }
  };
  // Containers in each folder: the ones listed by name first, then any a
  // folder's name pattern matches. A container is only ever in one folder.
  const grouped = $derived.by(() => {
    const all = list || [];
    const byName = Object.fromEntries(all.map((c) => [c.name, c]));
    const placed = new Set();
    const groups = folders.map((f) => ({ folder: f, members: [] }));
    for (const g of groups) {
      for (const n of g.folder.containers) {
        if (byName[n] && !placed.has(n)) (g.members.push(byName[n]), placed.add(n));
      }
    }
    for (const g of groups) {
      const re = patternOf(g.folder);
      if (re) for (const c of all) if (!placed.has(c.name) && re.test(c.name)) (g.members.push(c), placed.add(c.name));
    }
    return { groups, rest: all.filter((c) => !placed.has(c.name)) };
  });
  const folderOf = (name) => grouped.groups.find((g) => g.members.some((c) => c.name === name))?.folder;
  const shownRest = $derived(grouped.rest.filter(visible));

  function moveTo(name, folderId) {
    const next = folders.map((f) => ({ ...f, containers: f.containers.filter((n) => n !== name) }));
    const target = next.find((f) => f.id === folderId);
    if (target) target.containers = [...target.containers, name];
    saveFolders(next).then((ok) => {
      if (!ok) return;
      const still = folderOf(name);
      if (!folderId && still) toast(`${name} still matches ${still.name}'s name pattern, so it stays there.`, 'info', 6000);
    });
  }
  function toggle(f) {
    saveFolders(folders.map((x) => (x.id === f.id ? { ...x, collapsed: !x.collapsed } : x)));
  }
  function shift(f, d) {
    const i = folders.findIndex((x) => x.id === f.id);
    const j = i + d;
    if (j < 0 || j >= folders.length) return;
    const next = [...folders];
    [next[i], next[j]] = [next[j], next[i]];
    saveFolders(next);
  }
  async function deleteFolder(f) {
    const ok = await confirm({ title: `Delete the ${f.name} folder?`, message: 'Only the folder goes; its containers are left as they are.', confirmLabel: 'Delete folder', danger: true });
    if (ok) saveFolders(folders.filter((x) => x.id !== f.id));
  }
  async function folderAction(g, action) {
    const verbs = { start: ['Start', 'Started'], stop: ['Stop', 'Stopped'], restart: ['Restart', 'Restarted'] }[action];
    let targets = g.members.filter((c) => (action === 'start' ? c.state !== 'running' && c.state !== 'paused' : c.state === 'running'));
    if (!targets.length) {
      toast(action === 'start' ? 'Everything in it is already running.' : 'Nothing in it is running.', 'info', 3000);
      return;
    }
    if (action !== 'start') {
      const ok = await confirm({ title: `${verbs[0]} ${targets.length} container${targets.length === 1 ? '' : 's'} in ${g.folder.name}?`, message: targets.map((c) => c.name).join(', '), confirmLabel: verbs[0] });
      if (!ok) return;
    }
    if (action === 'stop') targets = [...targets].reverse(); // the opposite order to starting
    busyFolder = g.folder.id;
    const failed = [];
    for (const c of targets) {
      try {
        await api.post(`/containers/${c.id}/action/${action}`);
      } catch (e) {
        failed.push(`${c.name}: ${e.message}`);
      }
    }
    busyFolder = '';
    bumpLive();
    if (failed.length) toast(failed.join(' · '), 'error', 9000);
    else toast(`${verbs[1]} ${targets.length} in ${g.folder.name}`, 'success', 2500);
  }
  function folderMenu(g, i) {
    return [
      { label: 'Edit folder', icon: 'settings', onclick: () => openEditor(g.folder) },
      { label: 'Restart all', icon: 'restart', onclick: () => folderAction(g, 'restart') },
      { divider: true },
      i > 0 ? { label: 'Move up', icon: 'chevron-up', onclick: () => shift(g.folder, -1) } : null,
      i < folders.length - 1 ? { label: 'Move down', icon: 'chevron-down', onclick: () => shift(g.folder, 1) } : null,
      { divider: true },
      { label: 'Delete folder', icon: 'trash', danger: true, onclick: () => deleteFolder(g.folder) },
    ];
  }
  const usage = (members) => {
    const live = members.filter((c) => c.cpu != null);
    return live.length ? { cpu: live.reduce((a, c) => a + c.cpu, 0), mem: live.reduce((a, c) => a + c.mem, 0) } : null;
  };

  // Drag a row onto a folder (or onto "Not in a folder").
  function dragOver(e, id) {
    if (!e.dataTransfer.types.includes('text/x-container')) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    dropOn = id;
  }
  function drop(e, id) {
    e.preventDefault();
    const name = e.dataTransfer.getData('text/x-container');
    dropOn = null;
    if (name && folderOf(name)?.id !== (id || undefined)) moveTo(name, id);
  }

  // ---------- Folder editor ----------
  const folderIcons = ['folder', 'film', 'music', 'download', 'image', 'book', 'home', 'globe', 'network', 'activity', 'shield', 'database', 'archive', 'briefcase', 'git', 'sparkles', 'gamepad', 'wrench', 'layers', 'box'];
  const folderColors = ['', '#60a5fa', '#34d399', '#fbbf24', '#f472b6', '#a78bfa', '#22d3ee', '#fb923c', '#a3e635', '#f87171'];
  function openEditor(f) {
    editing = f ? { ...f, containers: [...f.containers] } : { id: '', name: '', icon: 'folder', color: '', match: '', containers: [], collapsed: false };
  }
  const editRe = $derived(editing ? patternOf(editing) : null);
  const editPatternOk = $derived(!editing?.match || !!editRe);
  function editToggle(name) {
    editing.containers = editing.containers.includes(name) ? editing.containers.filter((n) => n !== name) : [...editing.containers, name];
  }
  async function saveEditor(e) {
    e.preventDefault();
    if (!editPatternOk) return;
    const f = { ...editing, name: editing.name.trim() };
    let next = folders.map((x) => (x.id === f.id ? f : { ...x, containers: x.containers.filter((n) => !f.containers.includes(n)) }));
    if (!f.id) next = [...next, f];
    if (await saveFolders(next)) editing = null;
  }
</script>

{#snippet row(c, inner)}
  {@const running = c.state === 'running'}
  <tr class="clickable" class:inner draggable="true"
    ondragstart={(e) => { e.dataTransfer.setData('text/x-container', c.name); e.dataTransfer.effectAllowed = 'move'; }}
    ondragend={() => (dropOn = null)}
    onclick={() => navigate(`/containers/${c.id.slice(0, 12)}`)}>
    <td>
      <div class="row"><span class="dot {stateColor(c.state, c.health)}"></span><strong class="truncate nm">{c.name}</strong></div>
    </td>
    <td class="hide-sm">{#if c.stack}<a href="/stacks/{c.stack}" onclick={(e) => e.stopPropagation()}>{c.stack}</a>{:else}<span class="dim">–</span>{/if}</td>
    <td class="hide-sm"><span class="mono truncate img" title={c.image}>{c.image}</span>{#if c.updateAvailable}<span class="badge amber tiny">Update</span>{/if}</td>
    <td class="nowrap">
      {containerStateLabel(c.state)}{#if c.health}<span class="badge tiny {c.health === 'healthy' ? 'green' : c.health === 'unhealthy' ? 'red' : 'amber'}">{c.health}</span>{/if}
      <div class="dim small">{c.cpu != null ? `${percent(c.cpu)} · ${bytes(c.mem)}` : c.status}</div>
    </td>
    <td>
      <div class="ports">
        {#if c.webUI}<a class="port web" href={appUrl(c.webUI)} target="_blank" rel="noopener noreferrer" title={appUrl(c.webUI)} onclick={(e) => e.stopPropagation()}><Icon name="globe" size={11} /> Open</a>{/if}
        {#each c.ports.filter((p) => p.public).slice(0, c.webUI ? 2 : 3) as p}
          <a class="port" href={portUrl(p)} target="_blank" rel="noreferrer" onclick={(e) => e.stopPropagation()}>{p.public}:{p.private}</a>
        {:else}{#if !c.webUI}<span class="dim">–</span>{/if}{/each}
      </div>
    </td>
    <td class="hide-sm nowrap dim">{ago(c.created)}</td>
    <td class="actions" onclick={(e) => e.stopPropagation()}>
      {#if running}
        <button class="ghost icon sm" title="Stop" aria-label="Stop" onclick={() => containerAction(c, 'stop')}><Icon name="stop" size={15} /></button>
      {:else if c.state !== 'paused'}
        <button class="ghost icon sm" title="Start" aria-label="Start" onclick={() => containerAction(c, 'start')}><Icon name="play" size={15} /></button>
      {/if}
      <Menu size="sm" items={items(c)} />
    </td>
  </tr>
{/snippet}

<div class="page">
  <div class="page-header">
    <div>
      <h1>Containers</h1>
      <p class="sub">Every container on this host, including ones started outside a stack. Drag a row onto a folder to file it.</p>
    </div>
    <button onclick={() => openEditor(null)}><Icon name="folder" size={15} /> New folder</button>
  </div>

  <div class="toolbar">
    <div class="search">
      <Icon name="search" size={15} />
      <input placeholder="Search by name, image or stack" bind:value={search} />
    </div>
    <div class="segmented">
      {#each [['all', 'All'], ['running', 'Running'], ['stopped', 'Stopped'], ['standalone', 'Not in a stack']] as [k, l]}
        <button class:active={filter === k} onclick={() => (filter = k)}>{l}</button>
      {/each}
    </div>
  </div>
  {#if error}<p class="error-text">{error}</p>{/if}

  <div class="card">
    <div class="table-wrap">
      <table class="table">
        <thead>
          <tr><th>Name</th><th class="hide-sm">Stack</th><th class="hide-sm">Image</th><th>State</th><th>Ports</th><th class="hide-sm">Created</th><th></th></tr>
        </thead>
        {#if list === null}
          <tbody>{#each Array(4) as _}<tr><td colspan="7"><div class="skeleton" style="height:16px;width:50%"></div></td></tr>{/each}</tbody>
        {:else}
          {#each grouped.groups as g, i (g.folder.id)}
            {@const f = g.folder}
            {@const members = g.members.filter(visible)}
            {@const open = !f.collapsed || (filtering && members.length > 0)}
            {@const up = g.members.filter((c) => c.state === 'running').length}
            {@const use = usage(g.members)}
            {#if !filtering || members.length}
              <tbody class="fgroup" style="--fc:{f.color || 'var(--accent)'}">
                <tr class="fhead" class:drop={dropOn === f.id} ondragover={(e) => dragOver(e, f.id)} ondragleave={() => dropOn === f.id && (dropOn = null)} ondrop={(e) => drop(e, f.id)} onclick={() => toggle(f)}>
                  <td colspan="7">
                    <div class="frow">
                      <Icon name={open ? 'chevron-down' : 'chevron-right'} size={15} class="dim" />
                      <span class="ficon"><Icon name={f.icon || 'folder'} size={15} /></span>
                      <strong class="truncate">{f.name}</strong>
                      <span class="dim small nowrap">{up} of {g.members.length} running</span>
                      {#if !open}
                        <span class="peek hide-sm">{#each g.members.slice(0, 16) as c}<span class="dot {stateColor(c.state, c.health)}" title={c.name}></span>{/each}</span>
                      {/if}
                      {#if use}<span class="dim small nowrap hide-sm">{percent(use.cpu)} · {bytes(use.mem)}</span>{/if}
                      <span class="spacer"></span>
                      <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
                      <div class="facts" onclick={(e) => e.stopPropagation()}>
                        {#if busyFolder === f.id}<Icon name="loader" size={15} class="spin dim" />{/if}
                        <button class="ghost icon sm" title="Start all" aria-label="Start all in {f.name}" disabled={!!busyFolder} onclick={() => folderAction(g, 'start')}><Icon name="play" size={14} /></button>
                        <button class="ghost icon sm" title="Stop all" aria-label="Stop all in {f.name}" disabled={!!busyFolder} onclick={() => folderAction(g, 'stop')}><Icon name="stop" size={14} /></button>
                        <Menu size="sm" items={folderMenu(g, i)} />
                      </div>
                    </div>
                  </td>
                </tr>
                {#if open}
                  {#each members as c (c.id)}{@render row(c, true)}{/each}
                  {#if !g.members.length}<tr class="inner"><td colspan="7" class="dim small">Empty. Drag containers here, or edit the folder to pick them.</td></tr>{/if}
                {/if}
              </tbody>
            {/if}
          {/each}
          <tbody>
            {#if folders.length && (shownRest.length || !filtering)}
              <tr class="fhead other" class:drop={dropOn === ''} ondragover={(e) => dragOver(e, '')} ondragleave={() => dropOn === '' && (dropOn = null)} ondrop={(e) => drop(e, '')}>
                <td colspan="7"><div class="frow"><span class="dim small">Not in a folder · {grouped.rest.length}</span></div></td>
              </tr>
            {/if}
            {#each shownRest as c (c.id)}{@render row(c, false)}{/each}
          </tbody>
        {/if}
      </table>
      {#if list && !list.filter(visible).length}
        <div class="empty"><p>{list.length ? 'No containers match.' : 'No containers yet.'}</p></div>
      {/if}
    </div>
  </div>
</div>

{#if moving}
  {@const cur = folderOf(moving.name)}
  <Modal title="Move {moving.name}" onclose={() => (moving = null)}>
    <div class="pick">
      {#each folders as f (f.id)}
        <button class="pickrow" class:on={cur?.id === f.id} style="--fc:{f.color || 'var(--accent)'}" onclick={() => { moveTo(moving.name, f.id); moving = null; }}>
          <span class="ficon"><Icon name={f.icon || 'folder'} size={15} /></span>{f.name}{#if cur?.id === f.id}<span class="dim small"> · here now</span>{/if}
        </button>
      {/each}
      <button class="pickrow" class:on={!cur} onclick={() => { moveTo(moving.name, null); moving = null; }}><span class="ficon none"><Icon name="x" size={14} /></span>No folder</button>
      <button class="pickrow" onclick={() => { const n = moving.name; moving = null; openEditor(null); editing.containers = [n]; }}><span class="ficon none"><Icon name="plus" size={14} /></span>New folder…</button>
    </div>
  </Modal>
{/if}

{#if editing}
  <Modal title={editing.id ? `Edit ${editing.name || 'folder'}` : 'New folder'} width={560} onclose={() => (editing = null)}>
    <form id="folderform" class="stack-v" onsubmit={saveEditor}>
      <label class="field"><span>Name</span><input bind:value={editing.name} placeholder="Media" required maxlength="60" /></label>
      <div class="field"><span>Icon and colour</span>
        <div class="icons">
          {#each folderIcons as ic}
            <button type="button" class="icbtn" class:on={editing.icon === ic} style="--fc:{editing.color || 'var(--accent)'}" onclick={() => (editing.icon = ic)} aria-label={ic}><Icon name={ic} size={16} /></button>
          {/each}
        </div>
        <div class="swatches">
          {#each folderColors as col}
            <button type="button" class="sw" class:on={editing.color === col} style="--sw:{col || 'var(--accent)'}" onclick={() => (editing.color = col)} aria-label={col || 'Accent colour'}></button>
          {/each}
        </div>
      </div>
      <div class="field"><span>Containers</span>
        <div class="checks">
          {#each [...(list || [])].sort((a, b) => a.name.localeCompare(b.name)) as c (c.id)}
            {@const other = folderOf(c.name)}
            <label class="chk">
              <input type="checkbox" checked={editing.containers.includes(c.name)} onchange={() => editToggle(c.name)} />
              <span class="dot {stateColor(c.state, c.health)}"></span>
              <span class="grow truncate">{c.name}</span>
              {#if other && other.id !== editing.id && !editing.containers.includes(c.name)}<span class="dim small">in {other.name}</span>{:else if c.stack}<span class="dim small">{c.stack}</span>{/if}
            </label>
          {/each}
        </div>
      </div>
      <label class="field"><span>Also add containers whose names match <span class="dim">(optional)</span></span>
        <input class="mono" bind:value={editing.match} placeholder="^(sonarr|radarr|prowlarr)" spellcheck="false" autocapitalize="off" />
        {#if !editPatternOk}<span class="error-text">That isn't a valid pattern.</span>{:else if editRe}<span class="hint">Matches {(list || []).filter((c) => editRe.test(c.name)).length} container(s) now, and any created later.</span>{:else}<span class="hint">A regular expression, so new containers land here by themselves (like FolderView's regex).</span>{/if}
      </label>
    </form>
    {#snippet footer()}
      <button onclick={() => (editing = null)}>Cancel</button>
      <button class="primary" type="submit" form="folderform" disabled={!editing.name.trim() || !editPatternOk}>Save</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .toolbar { display: flex; gap: 10px; margin-bottom: 14px; flex-wrap: wrap; }
  .toolbar .search { flex: 1 1 240px; max-width: 380px; }
  .nm { max-width: 260px; }
  .img { display: inline-block; max-width: 220px; vertical-align: middle; }
  .tiny { height: 18px; font-size: 11px; margin-left: 6px; }
  .small { font-size: 12px; }
  .ports { display: flex; gap: 4px; flex-wrap: wrap; }
  .fhead { cursor: pointer; background: color-mix(in srgb, var(--fc, var(--accent)) 6%, var(--surface)); }
  .fhead:hover { background: color-mix(in srgb, var(--fc, var(--accent)) 10%, var(--surface)); }
  .fhead.other { background: var(--surface-2); cursor: default; }
  .fhead.drop { outline: 2px dashed var(--fc, var(--accent)); outline-offset: -3px; }
  .fhead td { padding-top: 8px; padding-bottom: 8px; }
  .frow { display: flex; align-items: center; gap: 9px; min-width: 0; }
  .frow strong { font-size: 14px; }
  .ficon { width: 26px; height: 26px; border-radius: 7px; display: grid; place-items: center; flex: none; color: var(--fc, var(--accent)); background: color-mix(in srgb, var(--fc, var(--accent)) 16%, var(--surface)); }
  .ficon.none { color: var(--text-3); background: var(--surface-2); }
  .peek { display: inline-flex; gap: 5px; margin-left: 4px; }
  .peek .dot { box-shadow: none; width: 7px; height: 7px; }
  .facts { display: flex; gap: 2px; align-items: center; }
  tr.inner td:first-child { padding-left: 36px; border-left: 3px solid color-mix(in srgb, var(--fc, var(--accent)) 55%, transparent); }
  tr[draggable='true'] { cursor: grab; }
  .pick { display: grid; gap: 4px; }
  .pickrow { justify-content: flex-start; gap: 10px; height: 40px; border-color: transparent; background: none; }
  .pickrow:hover { background: var(--surface-2); }
  .pickrow.on { background: var(--accent-soft); color: var(--accent-text); }
  .field { display: grid; gap: 7px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; color: var(--text); }
  .icons { display: flex; flex-wrap: wrap; gap: 6px; }
  .icbtn { width: 36px; height: 36px; padding: 0; display: grid; place-items: center; color: var(--text-2); }
  .icbtn.on { color: var(--fc); border-color: var(--fc); background: color-mix(in srgb, var(--fc) 14%, var(--surface)); }
  .swatches { display: flex; gap: 8px; flex-wrap: wrap; }
  .sw { width: 24px; height: 24px; padding: 0; border-radius: 50%; background: var(--sw); border: 2px solid var(--surface); box-shadow: 0 0 0 1px var(--border-strong); }
  .sw.on { box-shadow: 0 0 0 2px var(--text); }
  .checks { max-height: 260px; overflow: auto; border: 1px solid var(--border); border-radius: 10px; padding: 4px; display: grid; }
  .chk { display: flex; align-items: center; gap: 9px; padding: 6px 8px; border-radius: 7px; cursor: pointer; font-size: 13.5px; color: var(--text); min-width: 0; }
  .chk:hover { background: var(--surface-2); }
  .chk .dot { box-shadow: none; width: 7px; height: 7px; }
  .grow { flex: 1; min-width: 0; }
</style>
