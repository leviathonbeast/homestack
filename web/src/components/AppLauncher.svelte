<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { live, toast } from '../lib/state.svelte.js';
  import { appUrl, appUrlHint, nameColor, stateColor } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from './Modal.svelte';

  let data = $state(null);
  let editing = $state(false);
  let draft = $state([]);
  let saving = $state(false);
  let failedIcons = $state({});

  async function load() {
    try {
      data = await api.get('/apps');
    } catch {
      /* the dashboard shows Docker errors already */
    }
  }
  onMount(load);
  $effect(() => {
    live.tick;
    load();
  });

  function openEditor() {
    draft = (data?.apps || []).map((a) => ({ ...a }));
    editing = true;
  }
  function addSuggestion(s) {
    draft = [...draft, { id: '', name: s.name, url: s.url, container: s.container }];
  }
  function addCustom() {
    draft = [...draft, { id: '', name: '', url: 'http://[HOST]:', container: '' }];
  }
  function move(i, d) {
    const j = i + d;
    if (j < 0 || j >= draft.length) return;
    const copy = [...draft];
    [copy[i], copy[j]] = [copy[j], copy[i]];
    draft = copy;
  }
  async function save(list) {
    saving = true;
    try {
      data = await api.put('/apps', { apps: list });
      failedIcons = {};
      editing = false;
      toast('Saved', 'success', 1500);
    } catch (e) {
      toast(e.message, 'error', 6000);
    }
    saving = false;
  }
  async function addAll() {
    await save([...(data.apps || []), ...data.suggestions.map((s) => ({ id: '', name: s.name, url: s.url, container: s.container }))]);
  }

  const unused = $derived((data?.suggestions || []).filter((s) => !draft.some((d) => d.container && d.container === s.container)));
  const initial = (name) => (name || '?').trim().charAt(0).toUpperCase();
</script>

{#if data && (data.apps.length || data.suggestions.length)}
  <section class="card apps">
    <div class="card-header">
      <h2>Apps</h2>
      <span class="spacer"></span>
      <button class="sm ghost" onclick={openEditor}><Icon name="settings" size={14} /> Edit</button>
    </div>

    {#if data.apps.length}
      <div class="grid">
        {#each data.apps as a (a.id)}
          <a class="app" href={appUrl(a.url)} target="_blank" rel="noopener noreferrer" title={appUrl(a.url)}>
            <span class="icon" style="--c: {nameColor(a.name)}">
              {#if !failedIcons[a.id]}
                <img src="/api/apps/{a.id}/icon" alt="" loading="lazy" onerror={() => (failedIcons = { ...failedIcons, [a.id]: true })} />
              {:else}
                {initial(a.name)}
              {/if}
            </span>
            <span class="meta">
              <strong class="truncate">{a.name}</strong>
              <small class="truncate">{appUrlHint(a.url)}</small>
            </span>
            {#if a.state}<span class="dot {a.state === 'missing' ? 'gray' : stateColor(a.state)}" title={a.state === 'missing' ? 'Container not found' : a.state}></span>{/if}
          </a>
        {/each}
      </div>
    {:else}
      <div class="intro">
        <p>Homestack found {data.suggestions.length} app{data.suggestions.length === 1 ? '' : 's'} with a web page. Add shortcuts to them here.</p>
        <div class="row wrap">
          <button class="primary sm" onclick={addAll} disabled={saving}><Icon name="plus" size={14} /> Add all {data.suggestions.length}</button>
          <button class="sm" onclick={openEditor}>Choose…</button>
        </div>
      </div>
    {/if}
  </section>
{/if}

{#if editing}
  <Modal title="Edit app links" width={720} onclose={() => (editing = false)}>
    <div class="editor">
      {#if draft.length}
        <div class="rows">
          {#each draft as d, i}
            <div class="erow">
              <div class="order">
                <button class="ghost icon xs" aria-label="Move up" disabled={i === 0} onclick={() => move(i, -1)}><span class="up"><Icon name="arrowdown" size={13} /></span></button>
                <button class="ghost icon xs" aria-label="Move down" disabled={i === draft.length - 1} onclick={() => move(i, 1)}><Icon name="arrowdown" size={13} /></button>
              </div>
              <input class="name" bind:value={d.name} placeholder="Name" maxlength="60" aria-label="Name" />
              <input class="url mono" bind:value={d.url} placeholder="http://[HOST]:8080/" aria-label="Web address" spellcheck="false" />
              <button class="ghost icon sm" aria-label="Remove" onclick={() => (draft = draft.filter((_, k) => k !== i))}><Icon name="trash" size={14} /></button>
            </div>
          {/each}
        </div>
      {:else}
        <p class="dim small">No links yet. Add some from the list below.</p>
      {/if}
      <p class="hint"><code>[HOST]</code> becomes whatever address you opened Homestack with, so the same link works at home and over WireGuard. You can also use a full address like <code>https://jellyfin.home.lan/</code>.</p>

      {#if unused.length}
        <h3>Found on this server</h3>
        <div class="sugs">
          {#each unused as s}
            <button class="sug" onclick={() => addSuggestion(s)}>
              <Icon name="plus" size={13} /> <strong>{s.name}</strong> <span class="dim mono">{appUrlHint(s.url)}</span>
            </button>
          {/each}
        </div>
      {/if}
      <div><button class="sm" onclick={addCustom}><Icon name="plus" size={14} /> Add a custom link</button></div>
    </div>
    {#snippet footer()}
      <button onclick={() => (editing = false)}>Cancel</button>
      <button class="primary" onclick={() => save(draft)} disabled={saving}>Save</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .apps { margin-bottom: 20px; }
  .spacer { flex: 1; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(190px, 1fr)); gap: 8px; padding: 12px; }
  .app { display: flex; align-items: center; gap: 11px; padding: 10px 12px; border-radius: 10px; border: 1px solid var(--border); color: var(--text); min-width: 0; transition: border-color 0.12s, background 0.12s; }
  .app:hover { text-decoration: none; border-color: var(--border-strong); background: var(--surface-2); }
  .icon { width: 36px; height: 36px; flex: none; border-radius: 9px; display: grid; place-items: center; overflow: hidden;
    background: color-mix(in srgb, var(--c) 16%, var(--surface)); color: var(--c); font-weight: 600; font-size: 16px; }
  .icon img { width: 26px; height: 26px; object-fit: contain; }
  .meta { display: grid; min-width: 0; flex: 1; }
  .meta strong { font-size: 13.5px; font-weight: 500; }
  .meta small { font-size: 12px; color: var(--text-3); }
  .intro { padding: 14px 18px; display: grid; gap: 10px; }
  .intro p { margin: 0; color: var(--text-2); font-size: 13.5px; }
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .editor { display: grid; gap: 12px; }
  .rows { display: grid; gap: 6px; }
  .erow { display: grid; grid-template-columns: auto minmax(0, 0.8fr) minmax(0, 1.4fr) auto; gap: 6px; align-items: center; }
  .order { display: flex; flex-direction: column; }
  .order button { height: 16px; width: 22px; padding: 0; }
  .up { display: inline-flex; transform: rotate(180deg); }
  h3 { margin: 4px 0 0; font-size: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: var(--text-3); }
  .sugs { display: flex; flex-wrap: wrap; gap: 6px; }
  .sug { height: auto; padding: 5px 10px; gap: 6px; font-size: 13px; }
  .small { font-size: 13px; }
  @media (max-width: 640px) {
    .erow { grid-template-columns: auto 1fr auto; }
    .erow .url { grid-column: 2 / 3; }
  }
</style>
