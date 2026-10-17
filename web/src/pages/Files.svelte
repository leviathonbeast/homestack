<script>
  import { onMount } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import { api } from '../lib/api.js';
  import { toast, confirm } from '../lib/state.svelte.js';
  import { bytes, dateTime, duration } from '../lib/format.js';
  import { editorLanguage, maxEditBytes } from '../lib/editable.js';
  import { clip, setClip, clearClip } from '../lib/fileclip.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from '../components/Modal.svelte';
  import Menu from '../components/Menu.svelte';
  import ContextMenu from '../components/ContextMenu.svelte';
  import CodeEditor from '../components/CodeEditor.svelte';

  let data = $state(null);
  let roots = $state([]);
  let owner = $state('nobody:users'); // who owns the shares' files (Settings → Storage)
  let error = $state('');
  let loading = $state(false);
  let filter = $state('');
  let prompt = $state(null); // { title, label, value, action }
  const selected = new SvelteSet(); // names in this folder
  let lastPicked = -1; // for shift-click ranges
  let menu = $state(null); // the right-click menu: { x, y, items }
  let review = $state(null); // a copy or move waiting for OK: { plan, paths, op, dest, conflict, busy }
  let job = $state(null); // the copy or move running now

  // The file open in the editor: { path, name, content, saved, eol, stamp, size, lang }
  let editing = $state(null);
  let saving = $state(false);
  const dirty = $derived(!!editing && editing.content !== editing.saved);

  function fromUrl() {
    const q = new URLSearchParams(location.search);
    return { path: q.get('path') || '', edit: q.get('edit') || '' };
  }
  const fileUrl = (dir, name) => '/files?path=' + encodeURIComponent(dir) + (name ? '&edit=' + encodeURIComponent(name) : '');

  async function open(path, push = true) {
    if (!(await leaveEditor())) return;
    loading = true;
    try {
      const d = await api.get('/host/files' + (path ? '?path=' + encodeURIComponent(path) : ''));
      roots = d.roots || [];
      if (d.owner) owner = d.owner;
      if (!path) {
        data = null;
        if (roots.length) return open(roots[0].path, false);
      } else {
        if (d.path !== data?.path) {
          selected.clear();
          lastPicked = -1;
        } else {
          for (const n of selected) if (!d.entries.some((e) => e.name === n)) selected.delete(n);
        }
        data = d;
        filter = '';
        if (push) history.pushState({}, '', fileUrl(path));
      }
      error = '';
    } catch (e) {
      error = e.message;
    }
    loading = false;
  }
  onMount(() => {
    const { path, edit } = fromUrl();
    open(path, false).then(() => edit && data && openEditor(edit, false));
    const pop = async () => {
      const u = fromUrl();
      if (dirty) {
        // Going back would lose the edits: stay, and keep the editor's URL.
        history.pushState({}, '', fileUrl(data.path, editing.name));
        toast('Save or discard your changes first.', 'warn', 4000);
        return;
      }
      editing = null;
      if (u.path !== data?.path) await open(u.path, false);
      if (u.edit) openEditor(u.edit, false);
    };
    const unload = (e) => {
      if (dirty) e.preventDefault();
    };
    addEventListener('popstate', pop);
    addEventListener('beforeunload', unload);
    watchJob(); // a copy or move may be running already
    return () => {
      removeEventListener('popstate', pop);
      removeEventListener('beforeunload', unload);
      clearTimeout(jobTimer);
    };
  });

  const crumbs = $derived.by(() => {
    if (!data) return [];
    const out = [{ name: data.root.label, path: data.root.path }];
    const rest = data.path.slice(data.root.path.length).split('/').filter(Boolean);
    let p = data.root.path;
    for (const r of rest) out.push({ name: r, path: (p += '/' + r) });
    return out;
  });
  const shown = $derived((data?.entries || []).filter((e) => !filter || e.name.toLowerCase().includes(filter.toLowerCase())));
  const join = (name) => (data.path === '/' ? '' : data.path) + '/' + name;

  async function fileOp(body, msg) {
    try {
      await api.post('/host/files', body);
      if (msg) toast(msg, 'success', 2000);
      await open(data.path, false);
      return true;
    } catch (e) {
      toast(e.message, 'error', 8000);
      return false;
    }
  }
  function newFolder() {
    prompt = { title: 'New folder', label: 'Name', value: '', action: (name) => fileOp({ op: 'mkdir', path: data.path, name }, 'Folder created') };
  }
  function newFile() {
    prompt = {
      title: 'New file', label: 'Name', value: '', placeholder: 'notes.txt',
      action: async (name) => (await fileOp({ op: 'mkfile', path: data.path, name })) && openEditor(name),
    };
  }
  function rename(e) {
    prompt = { title: `Rename ${e.name}`, label: 'New name', value: e.name, action: (name) => fileOp({ op: 'rename', path: join(e.name), name }, 'Renamed') };
  }
  async function del(e) {
    const ok = await confirm({
      title: `Delete ${e.name}?`,
      message: e.dir ? 'The folder and everything inside it are deleted. This can\'t be undone here (parity can bring back files deleted since the last sync).' : 'The file is deleted. This can\'t be undone here.',
      confirmLabel: 'Delete', danger: true, typeToConfirm: e.dir ? e.name : '',
    });
    if (ok) fileOp({ op: 'delete', path: join(e.name), confirm: e.dir ? e.name : '' }, 'Deleted');
  }
  async function perms(path) {
    const ok = await confirm({
      title: 'New permissions?',
      message: `Sets owner ${owner}, folders 777 and files 666 for everything in ${path}, like Unraid's New Permissions. Fixes "access denied" for shares and containers. Runs as a job; big folders take a while.`,
      confirmLabel: 'Run',
    });
    if (!ok) return;
    try {
      await api.post('/host/actions', { action: 'fix-permissions', path });
      toast('Started. Progress is on the Storage page.', 'success', 4000);
    } catch (err) {
      toast(err.message, 'error', 8000);
    }
  }
  function submitPrompt(ev) {
    ev.preventDefault();
    const p = prompt;
    prompt = null;
    p.action(p.value.trim());
  }
  const up = $derived(crumbs.length > 1 ? crumbs[crumbs.length - 2].path : null);
  const downloadUrl = (path) => '/api/host/files/download?path=' + encodeURIComponent(path);

  // ---------- Editor ----------

  async function openEditor(name, push = true) {
    const path = join(name);
    try {
      const f = await api.get('/host/files/content?path=' + encodeURIComponent(path));
      editing = { path, name, content: f.content, saved: f.content, eol: f.eol, stamp: f.stamp, size: f.size, lang: editorLanguage(name) || 'text' };
      if (push) history.pushState({}, '', fileUrl(data.path, name));
    } catch (e) {
      toast(e.message, 'error', 8000);
      if (!push) history.replaceState({}, '', fileUrl(data.path));
    }
  }
  // leaveEditor asks before throwing away unsaved changes; true = go ahead.
  async function leaveEditor() {
    if (!editing) return true;
    if (dirty && !(await confirm({ title: `Discard changes to ${editing.name}?`, message: 'Your edits haven\'t been saved.', confirmLabel: 'Discard', danger: true }))) return false;
    editing = null;
    return true;
  }
  async function closeEditor() {
    if (await leaveEditor()) history.pushState({}, '', fileUrl(data.path));
  }
  async function save(force = false) {
    if (!editing || saving || (!dirty && !force)) return;
    saving = true;
    const ed = editing;
    const content = ed.content;
    try {
      const r = await api.put('/host/files/content', { path: ed.path, content, eol: ed.eol, stamp: ed.stamp, force });
      Object.assign(ed, { saved: content, stamp: r.stamp, size: r.size });
      toast(`Saved ${ed.name}`, 'success', 1800);
    } catch (e) {
      if (e.status === 409) {
        saving = false;
        const ok = await confirm({
          title: `${ed.name} changed on disk`,
          message: 'Something else saved this file after you opened it. Overwrite it with your version, or cancel and copy your changes somewhere first?',
          confirmLabel: 'Overwrite', danger: true,
        });
        if (ok) return save(true);
        return;
      }
      toast(e.message, 'error', 8000);
    }
    saving = false;
  }
  async function reload() {
    if (dirty && !(await confirm({ title: 'Reload from disk?', message: 'Your unsaved changes are lost.', confirmLabel: 'Reload', danger: true }))) return;
    const name = editing.name;
    editing = null;
    openEditor(name, false);
  }
  const canEdit = (e) => !e.dir && e.size <= maxEditBytes;

  // ---------- Selecting, copy, cut and paste ----------

  const picked = $derived(shown.filter((e) => selected.has(e.name)));
  const allPicked = $derived(shown.length > 0 && shown.every((e) => selected.has(e.name)));
  const named = (list) => (list.length === 1 ? list[0].name : `${list.length} items`);
  const inDir = (dir, name) => (dir === '/' ? '' : dir) + '/' + name;

  function pickAll() {
    if (allPicked) selected.clear();
    else for (const e of shown) selected.add(e.name);
  }
  // Click a row to pick it; Ctrl (⌘) adds or removes, Shift picks a range.
  function pick(e, ev, index) {
    if (ev.shiftKey && lastPicked >= 0) {
      const [a, b] = [Math.min(lastPicked, index), Math.max(lastPicked, index)];
      for (const x of shown.slice(a, b + 1)) selected.add(x.name);
    } else if (ev.ctrlKey || ev.metaKey || ev.target.type === 'checkbox') {
      if (selected.has(e.name)) selected.delete(e.name);
      else selected.add(e.name);
    } else {
      const only = selected.size === 1 && selected.has(e.name);
      selected.clear();
      if (!only) selected.add(e.name);
    }
    lastPicked = index;
  }
  function rowClick(e, ev, index) {
    if (ev.target.closest('a, button, input')) return; // names and buttons do their own thing
    pick(e, ev, index);
  }

  function toClip(list, op) {
    if (!list.length) return;
    setClip(op, data.path, list.map((e) => e.name));
    selected.clear();
    toast(`${op === 'move' ? 'Cut' : 'Copied'} ${named(list)}. Open the folder ${list.length > 1 ? 'they go' : 'it goes'} in, then Paste.`, 'info', 4000);
  }
  const clipNamed = $derived(clip.names.length === 1 ? clip.names[0] : `${clip.names.length} items`);

  // Paste asks the agent what it would do first, and shows that for an OK.
  async function paste(dest = data.path) {
    if (!clip.op) return;
    const paths = clip.names.map((n) => inDir(clip.from, n));
    try {
      const plan = await api.post('/host/files/plan', { op: clip.op, paths, dest, conflict: 'skip' });
      review = { plan, paths, op: clip.op, dest, conflict: 'skip', busy: false };
    } catch (e) {
      toast(e.message, 'error', 10000);
    }
  }
  async function startTransfer() {
    const r = review;
    r.busy = true;
    try {
      const res = await api.post('/host/actions', { action: `file-${r.op}`, paths: r.paths, dest: r.dest, conflict: r.conflict });
      if (r.op === 'move') clearClip(); // a copy can be pasted again elsewhere
      review = null;
      job = res.job;
      watchJob();
    } catch (e) {
      r.busy = false;
      toast(e.message, 'error', 10000);
    }
  }

  // A copy or move runs on the server as a storage job: follow it here.
  let jobTimer;
  let jobId = '';
  async function watchJob() {
    clearTimeout(jobTimer);
    try {
      const st = await api.get('/host/status');
      const r = st.running;
      if (r && (r.action === 'file-copy' || r.action === 'file-move')) {
        job = r;
        jobId = r.id;
        jobTimer = setTimeout(watchJob, 1500);
        return;
      }
      if (job || jobId) {
        const done = (st.jobs || []).find((j) => j.id === (jobId || job?.id));
        if (done) {
          const level = done.state === 'ok' ? 'success' : done.state === 'stopped' ? 'warn' : 'error';
          toast(`${done.title}: ${done.detail || done.state}`, level, done.state === 'ok' ? 5000 : 12000);
        }
        job = null;
        jobId = '';
        if (data && !editing) open(data.path, false);
      }
    } catch {
      if (job) jobTimer = setTimeout(watchJob, 4000);
    }
  }
  async function stopJob() {
    if (!(await confirm({ title: `Stop: ${job.title}?`, message: 'What has been copied stays; the file being copied right now is thrown away, so nothing is left half-written. Originals that weren\'t moved yet stay where they were.', confirmLabel: 'Stop' }))) return;
    try {
      await api.post('/host/actions', { action: 'job-stop' });
    } catch (e) {
      toast(e.message, 'error');
    }
  }

  async function delItems(list) {
    if (list.length === 1) return del(list[0]);
    const dirs = list.filter((e) => e.dir).length;
    const ok = await confirm({
      title: `Delete ${list.length} items?`,
      message: `${list.slice(0, 6).map((e) => e.name).join(', ')}${list.length > 6 ? ` and ${list.length - 6} more` : ''}. ${dirs ? 'Folders go with everything inside them. ' : ''}This can't be undone here (parity can bring back files deleted since the last sync).`,
      confirmLabel: 'Delete', danger: true, typeToConfirm: dirs ? 'DELETE' : '',
    });
    if (!ok) return;
    const failed = [];
    for (const e of list) {
      try {
        await api.post('/host/files', { op: 'delete', path: join(e.name), confirm: e.dir ? e.name : '' });
      } catch (err) {
        failed.push(`${e.name}: ${err.message}`);
      }
    }
    selected.clear();
    if (failed.length) toast(`${list.length - failed.length} deleted; ${failed.length} couldn't be. ${failed[0]}`, 'error', 10000);
    else toast(`Deleted ${list.length} items`, 'success', 2500);
    open(data.path, false);
  }

  function download(e) {
    const a = document.createElement('a');
    a.href = downloadUrl(join(e.name));
    a.download = e.name;
    a.click();
  }

  // What a right-click (or the ⋯ button) offers for some items.
  function itemsFor(list) {
    const one = list.length === 1 ? list[0] : null;
    const many = list.length > 1 ? ` ${list.length} items` : '';
    return [
      one?.dir ? { label: 'Open', icon: 'folder-open', onclick: () => open(join(one.name)) } : null,
      one && canEdit(one) ? { label: 'Edit as text', icon: 'code', onclick: () => openEditor(one.name) } : null,
      one && !one.dir ? { label: 'Download', icon: 'download', onclick: () => download(one) } : null,
      one ? { divider: true } : null,
      { label: `Copy${many}`, icon: 'copy', keys: 'Ctrl+C', onclick: () => toClip(list, 'copy') },
      { label: `Cut${many}`, icon: 'scissors', hint: 'To move: paste it in another folder', keys: 'Ctrl+X', onclick: () => toClip(list, 'move') },
      one?.dir && clip.op ? { label: `Paste into ${one.name}`, icon: 'clipboard', hint: `${clip.op === 'move' ? 'Move' : 'Copy'} ${clipNamed} there`, onclick: () => paste(join(one.name)) } : null,
      { divider: true },
      one ? { label: 'Rename', icon: 'pencil', onclick: () => rename(one) } : null,
      one?.dir ? { label: 'New permissions', icon: 'lock', onclick: () => perms(join(one.name)) } : null,
      { label: `Delete${many}`, icon: 'trash', danger: true, keys: 'Del', onclick: () => delItems(list) },
    ];
  }
  function rowMenu(ev, e, index) {
    ev.preventDefault();
    if (!selected.has(e.name)) {
      selected.clear();
      selected.add(e.name);
      lastPicked = index;
    }
    menu = { x: ev.clientX, y: ev.clientY, items: itemsFor(shown.filter((x) => selected.has(x.name))) };
  }
  function folderMenu(ev) {
    if (ev.target.closest('tr[data-name], input, textarea')) return;
    ev.preventDefault();
    selected.clear();
    menu = {
      x: ev.clientX, y: ev.clientY,
      items: [
        clip.op ? { label: `Paste ${clipNamed}`, icon: 'clipboard', keys: 'Ctrl+V', disabled: clip.from === data.path, onclick: () => paste() } : null,
        clip.op ? { divider: true } : null,
        { label: 'New file', icon: 'file', onclick: newFile },
        { label: 'New folder', icon: 'plus', onclick: newFolder },
        data.path !== data.root.path ? { label: 'New permissions', icon: 'lock', onclick: () => perms(data.path) } : null,
        { label: 'Refresh', icon: 'refresh', onclick: () => open(data.path, false) },
      ],
    };
  }

  // Keyboard: Ctrl+A, C, X, V, Delete and Escape, as in a desktop file manager.
  function onkey(ev) {
    if (!data || editing || review || prompt || menu || document.querySelector('[role=dialog]')) return;
    if (ev.target.closest?.('input, textarea, select, [contenteditable]')) return;
    const mod = ev.ctrlKey || ev.metaKey;
    const k = ev.key.toLowerCase();
    if (mod && k === 'a') {
      ev.preventDefault();
      for (const e of shown) selected.add(e.name);
    } else if (mod && (k === 'c' || k === 'x') && picked.length && !String(getSelection())) {
      ev.preventDefault();
      toClip(picked, k === 'c' ? 'copy' : 'move');
    } else if (mod && k === 'v' && clip.op && clip.from !== data.path) {
      ev.preventDefault();
      paste();
    } else if (ev.key === 'Delete' && picked.length) {
      ev.preventDefault();
      delItems(picked);
    } else if (ev.key === 'Escape' && selected.size) {
      selected.clear();
    }
  }
</script>

<svelte:window onkeydown={onkey} />

<div class="page">
  <div class="page-header">
    <div>
      <h1>Files</h1>
      <p class="sub">{data ? `${bytes(data.free)} free of ${bytes(data.size)} on this ${data.root.id === 'user' ? 'array view' : 'disk'}` : ' '}</p>
    </div>
    {#if data && !editing}
      <div class="row wrap">
        <button onclick={newFile}><Icon name="file" size={15} /> New file</button>
        <button onclick={newFolder}><Icon name="plus" size={15} /> New folder</button>
        {#if data.path !== data.root.path}<button onclick={() => perms(data.path)}><Icon name="lock" size={15} /> New permissions</button>{/if}
        <button class="ghost icon" aria-label="Refresh" onclick={() => open(data.path, false)}><Icon name="refresh" size={15} /></button>
      </div>
    {/if}
  </div>

  {#if roots.length}
    <div class="roots">
      {#each roots as r}
        <button class="chip" class:active={data?.root.id === r.id} onclick={() => open(r.path)}>{r.label}</button>
      {/each}
    </div>
  {/if}

  {#if error}
    <div class="note err"><Icon name="alert" size={16} /> {error}</div>
  {/if}

  {#if data && editing}
    <section class="card">
      <div class="bar">
        <button class="ghost icon sm" aria-label="Back to the folder" title="Back to the folder" onclick={closeEditor}><Icon name="chevron-left" size={15} /></button>
        <nav class="crumbs">
          {#each crumbs as c, i}
            {#if i}<span class="sep">/</span>{/if}
            <button class="crumb" onclick={() => open(c.path)}>{c.name}</button>
          {/each}
          <span class="sep">/</span><span class="crumb last">{editing.name}</span>
          {#if dirty}<span class="unsaved" title="Unsaved changes"></span>{/if}
        </nav>
        <span class="dim small nowrap hide-sm">{editing.lang === 'text' ? 'Plain text' : editing.lang}{editing.eol === 'crlf' ? ' · CRLF' : ''} · {bytes(editing.size)}</span>
        <a class="btn sm ghost icon" aria-label="Download" title="Download" href={downloadUrl(editing.path)} download><Icon name="download" size={14} /></a>
        <button class="sm ghost" onclick={reload} title="Load the file from disk again">Reload</button>
        {#if dirty}<button class="sm" onclick={() => (editing.content = editing.saved)}>Discard</button>{/if}
        <button class="sm primary" disabled={!dirty || saving} onclick={() => save()}>
          {#if saving}<Icon name="loader" size={14} class="spin" />{:else}<Icon name="save" size={14} />{/if} Save
        </button>
      </div>
      <div class="edwrap">
        {#key editing.path}
          <CodeEditor bind:value={editing.content} language={editing.lang} height="calc(100vh - 290px)" onsave={() => save()} />
        {/key}
      </div>
    </section>
    <p class="dim small tail">Ctrl+S (⌘S) saves. The file keeps its owner and permissions{editing.eol === 'crlf' ? ', and its Windows (CRLF) line endings' : ''}.</p>
  {:else if data}
    {#if job}
      <section class="card jobcard" aria-live="polite">
        <div class="jobhead">
          <Icon name="loader" size={16} class="spin" />
          <strong class="grow truncate">{job.title}</strong>
          <button class="sm" onclick={stopJob}><Icon name="stop" size={14} /> Stop</button>
        </div>
        <div class="meter"><div style="width:{job.progress ?? 0}%"></div></div>
        <div class="jobmeta">
          {#if job.doneBytes != null}<span>{bytes(job.doneBytes)} of {bytes(job.totalBytes)}</span>{/if}
          {#if job.speed}<span>{job.speed}</span>{/if}
          {#if job.etaSeconds != null}<span>{duration(job.etaSeconds)} left</span>{/if}
          {#if job.step}<span class="mono truncate grow" title={job.step}>{job.step}</span>{/if}
        </div>
      </section>
    {/if}
    {#if clip.op}
      <div class="clipbar">
        <Icon name={clip.op === 'move' ? 'scissors' : 'copy'} size={16} />
        <span class="grow">{clip.op === 'move' ? 'Moving' : 'Copying'} <strong>{clipNamed}</strong> from <code>{clip.from}</code>.{clip.from === data.path ? ' Open the folder to put it in, then paste.' : ''}</span>
        <button class="sm primary" disabled={clip.from === data.path} title={clip.from === data.path ? 'They are in this folder already' : `Paste into ${data.path}`} onclick={() => paste()}><Icon name="clipboard" size={14} /> Paste here</button>
        <button class="sm ghost" onclick={clearClip}>Cancel</button>
      </div>
    {/if}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <section class="card" oncontextmenu={folderMenu}>
      <div class="bar">
        <button class="ghost icon sm" aria-label="Up one folder" disabled={!up} onclick={() => up && open(up)}><Icon name="chevron-left" size={15} /></button>
        <nav class="crumbs">
          {#each crumbs as c, i}
            {#if i}<span class="sep">/</span>{/if}
            <button class="crumb" class:last={i === crumbs.length - 1} onclick={() => open(c.path)}>{c.name}</button>
          {/each}
        </nav>
        <div class="search"><Icon name="search" size={14} /><input placeholder="Filter" bind:value={filter} /></div>
      </div>
      {#if picked.length}
        <div class="selbar">
          <strong>{picked.length} selected</strong>
          <button class="sm ghost" onclick={() => toClip(picked, 'copy')}><Icon name="copy" size={14} /> Copy</button>
          <button class="sm ghost" onclick={() => toClip(picked, 'move')}><Icon name="scissors" size={14} /> Cut</button>
          <button class="sm ghost danger-text" onclick={() => delItems(picked)}><Icon name="trash" size={14} /> Delete</button>
          <span class="grow"></span>
          <button class="sm ghost" onclick={() => selected.clear()}>Clear</button>
        </div>
      {/if}
      {#if loading}<div class="loadbar"></div>{/if}
      <div class="table-wrap listing">
        <table class="table">
          <thead><tr>
            <th class="pick"><input type="checkbox" aria-label="Select all" checked={allPicked} indeterminate={picked.length > 0 && !allPicked} onchange={pickAll} disabled={!shown.length} /></th>
            <th>Name</th><th class="num">Size</th><th class="hide-sm">Modified</th><th class="hide-md">Owner</th><th class="hide-md">Permissions</th>{#if data.root.id === 'user'}<th class="hide-sm">On</th>{/if}<th></th>
          </tr></thead>
          <tbody>
            {#each shown as e, i (e.name)}
              <tr data-name={e.name} class:sel={selected.has(e.name)} onclick={(ev) => rowClick(e, ev, i)} oncontextmenu={(ev) => rowMenu(ev, e, i)}>
                <td class="pick"><input type="checkbox" aria-label="Select {e.name}" checked={selected.has(e.name)} onclick={(ev) => pick(e, ev, i)} /></td>
                <td class="name">
                  {#if e.dir}
                    <button class="link" onclick={() => open(join(e.name))}><Icon name="folder" size={15} /> {e.name}</button>
                  {:else if editorLanguage(e.name) && canEdit(e)}
                    <button class="link" onclick={() => openEditor(e.name)} title="Edit {e.name}"><Icon name="code" size={15} /> {e.name}</button>
                  {:else}
                    <a class="link" href={downloadUrl(join(e.name))} download><Icon name="file" size={15} /> {e.name}</a>
                  {/if}
                  {#if e.link}<span class="dim small"> → {e.link}</span>{/if}
                </td>
                <td class="num">{e.dir ? '' : bytes(e.size)}</td>
                <td class="hide-sm small dim nowrap">{dateTime(e.mtime)}</td>
                <td class="hide-md small">{e.owner}:{e.group}</td>
                <td class="hide-md mono small dim">{e.mode}</td>
                {#if data.root.id === 'user'}<td class="hide-sm small">{(e.disks || []).join(', ')}</td>{/if}
                <td class="actions">
                  {#if canEdit(e)}<button class="sm ghost icon" aria-label="Edit {e.name}" title="Edit as text" onclick={() => openEditor(e.name)}><Icon name="code" size={14} /></button>{/if}
                  {#if !e.dir}<a class="btn sm ghost icon" aria-label="Download {e.name}" href={downloadUrl(join(e.name))} download><Icon name="download" size={14} /></a>{/if}
                  <Menu size="sm" label="Actions for {e.name}" items={itemsFor([e])} />
                </td>
              </tr>
            {:else}
              <tr><td colspan="8" class="dim">{filter ? 'Nothing matches.' : 'This folder is empty.'}</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
      {#if data.truncated}<div class="card-body dim small">Showing the first 5,000 items. Use a share or disk to narrow it down.</div>{/if}
    </section>
    <p class="dim small tail"><strong>Right-click</strong> anything for Copy, Cut, Paste, Rename and Delete (or use <Icon name="more" size={12} />, Ctrl+C, Ctrl+X, Ctrl+V and Delete). Click rows to select several; Shift-click picks a range. Text files (configs, YAML, scripts, notes…) open in the editor; anything up to 2 MB can be opened as text with <Icon name="code" size={12} />. "On" shows which disk holds each item (a folder can be on several). Files deleted here are gone, but parity can restore anything deleted since the last sync (<code>snapraid fix -m</code>).</p>
  {:else if !error}
    <div class="skeleton" style="height:320px"></div>
  {/if}
</div>

{#if review}
  {@const p = review.plan}
  {@const verb = review.op === 'move' ? 'Move' : 'Copy'}
  <Modal title="{verb} {p.items === 1 ? review.paths[0].split('/').pop() : `${p.items} items`} here?" onclose={() => !review.busy && (review = null)} width={580}>
    <div class="stack-v">
      <dl class="kv">
        <dt>From</dt><dd class="mono">{review.paths[0].slice(0, review.paths[0].lastIndexOf('/')) || '/'}</dd>
        <dt>To</dt><dd class="mono">{p.dest}</dd>
        <dt>What</dt><dd>{p.files} file{p.files === 1 ? '' : 's'}{p.dirs ? ` in ${p.dirs} folder${p.dirs === 1 ? '' : 's'}` : ''} · {bytes(p.bytes)}{p.partial ? ' at least' : ''}</dd>
        {#if !p.instant}<dt>Free there</dt><dd>{bytes(p.free)}</dd>{/if}
      </dl>
      {#if review.op === 'move'}
        <p class="hint">{p.instant ? 'They’re on the same disk, so they’re moved instantly by renaming: nothing is copied.' : 'Each file is copied and saved to disk first; only then is its original deleted. Stopping halfway never loses a file.'}</p>
      {/if}
      {#if p.conflicts.length}
        <div class="note amber"><Icon name="alert" size={16} /><span><strong>{p.conflicts.length === 1 ? `${p.conflicts[0]} is` : `${p.conflicts.length} of them are`} already there</strong>{p.conflicts.length > 1 ? `: ${p.conflicts.slice(0, 5).join(', ')}${p.conflicts.length > 5 ? '…' : ''}` : ''}. Folders with the same name are merged; for files with the same name:</span></div>
        <div class="segmented">
          <button type="button" class:active={review.conflict === 'skip'} onclick={() => (review.conflict = 'skip')}>Keep what's there</button>
          <button type="button" class:active={review.conflict === 'replace'} onclick={() => (review.conflict = 'replace')}>Replace with these</button>
        </div>
      {/if}
      {#each p.warnings as w}<div class="note amber"><Icon name="info" size={16} /><span>{w}</span></div>{/each}
      <p class="hint">It runs on the server as a storage job, so it never overlaps the mover or a parity sync. You can leave this page: progress is here and on the Storage page.</p>
    </div>
    {#snippet footer()}
      <button onclick={() => (review = null)} disabled={review.busy}>Cancel</button>
      <button class="primary" onclick={startTransfer} disabled={review.busy}>
        {#if review.busy}<Icon name="loader" size={14} class="spin" />{:else}<Icon name={review.op === 'move' ? 'scissors' : 'copy'} size={14} />{/if} {verb}
      </button>
    {/snippet}
  </Modal>
{/if}

{#if menu}<ContextMenu x={menu.x} y={menu.y} items={menu.items} onclose={() => (menu = null)} />{/if}

{#if prompt}
  <Modal title={prompt.title} onclose={() => (prompt = null)}>
    <form id="promptform" onsubmit={submitPrompt}>
      <label class="field"><span>{prompt.label}</span><input bind:value={prompt.value} placeholder={prompt.placeholder || ''} required maxlength="255" /></label>
    </form>
    {#snippet footer()}
      <button onclick={() => (prompt = null)}>Cancel</button>
      <button class="primary" type="submit" form="promptform">OK</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .roots { display: flex; gap: 6px; flex-wrap: wrap; margin-bottom: 12px; }
  .chip { height: 30px; padding: 0 12px; border-radius: 99px; font-size: 13px; }
  .chip.active { background: var(--accent); border-color: var(--accent); color: var(--on-accent, #fff); }
  .note { display: flex; gap: 10px; padding: 11px 14px; border-radius: 10px; margin-bottom: 12px; font-size: 13.5px; }
  .note.err { background: var(--red-soft); color: var(--red-text); }
  .bar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--border); }
  .crumbs { flex: 1; min-width: 0; display: flex; flex-wrap: wrap; align-items: center; gap: 2px; font-size: 13.5px; }
  .crumb { border: 0; background: none; height: auto; padding: 2px 4px; color: var(--accent); }
  .crumb.last { color: var(--text); font-weight: 600; }
  .sep { color: var(--text-3); }
  .unsaved { width: 7px; height: 7px; border-radius: 50%; background: var(--amber); margin-left: 6px; }
  .search input { height: 30px; width: 170px; }
  .loadbar { height: 2px; background: var(--accent); animation: pulse 1s infinite alternate; }
  @keyframes pulse { from { opacity: 0.2; } to { opacity: 1; } }
  .name { max-width: 460px; }
  .link { display: inline-flex; align-items: center; gap: 7px; border: 0; background: none; padding: 0; height: auto; color: var(--text); text-align: left; overflow-wrap: anywhere; }
  .link:hover { color: var(--accent); text-decoration: none; }
  .link :global(svg) { flex: none; color: var(--text-3); }
  .num { text-align: right; white-space: nowrap; }
  .small { font-size: 12.5px; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  .nowrap { white-space: nowrap; }
  .actions { text-align: right; white-space: nowrap; }
  .tail { margin-top: 14px; }
  .field { display: grid; gap: 6px; }
  .listing { min-height: 220px; }
  .pick { width: 34px; padding-right: 0 !important; }
  .pick input { margin: 0; cursor: pointer; }
  tbody tr { cursor: default; }
  tbody tr.sel { background: var(--accent-soft); }
  tbody tr.sel:hover { background: color-mix(in srgb, var(--accent-soft) 80%, var(--accent) 6%); }
  .selbar { display: flex; align-items: center; gap: 6px; padding: 6px 14px; border-bottom: 1px solid var(--border); background: var(--accent-soft); font-size: 13px; flex-wrap: wrap; }
  .selbar strong { margin-right: 6px; color: var(--accent-text); }
  .danger-text { color: var(--red-text); }
  .clipbar { display: flex; align-items: center; gap: 10px; padding: 10px 14px; margin-bottom: 12px; border-radius: 10px; background: var(--accent-soft); color: var(--accent-text); font-size: 13.5px; flex-wrap: wrap; }
  .clipbar :global(svg) { flex: none; }
  .clipbar code { color: inherit; }
  .jobcard { padding: 12px 16px; margin-bottom: 12px; display: grid; gap: 8px; }
  .jobhead { display: flex; align-items: center; gap: 10px; min-width: 0; }
  .jobmeta { display: flex; gap: 14px; font-size: 12.5px; color: var(--text-2); min-width: 0; flex-wrap: wrap; }
  .grow { flex: 1; min-width: 0; }
  .kv { display: grid; grid-template-columns: max-content 1fr; gap: 7px 16px; margin: 0; font-size: 13.5px; }
  .kv dt { color: var(--text-2); }
  .kv dd { margin: 0; overflow-wrap: anywhere; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); margin: 0; font-size: 13px; line-height: 1.5; }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .hint { margin: 0; }
  .edwrap { padding: 10px; }
  .edwrap :global(.editor) { min-height: 360px; }
  @media (max-width: 900px) { :global(.hide-md) { display: none; } }
</style>
