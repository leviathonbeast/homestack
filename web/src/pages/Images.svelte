<script>
  import { api } from '../lib/api.js';
  import { live, toast, confirm, runJob, bumpLive } from '../lib/state.svelte.js';
  import { bytes, ago, shortId } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';

  let list = $state(null);
  let search = $state('');
  let pullRef = $state('');
  let filter = $state('all');
  let updates = $state(null);

  function load() {
    api.get('/images').then((l) => (list = l)).catch((e) => toast(e.message, 'error'));
    api.get('/updates').then((u) => (updates = u)).catch(() => {});
  }
  $effect(() => {
    live.tick;
    load();
  });

  const shown = $derived(
    (list || []).filter((i) => {
      if (search && !(i.tags.join(' ') + i.id).toLowerCase().includes(search.toLowerCase())) return false;
      if (filter === 'unused') return i.inUse === 0;
      if (filter === 'updates') return i.updateAvailable;
      return true;
    }),
  );
  const total = $derived((list || []).reduce((a, i) => a + i.size, 0));
  const unusedSize = $derived((list || []).filter((i) => !i.inUse).reduce((a, i) => a + i.size, 0));

  async function pull(e) {
    e?.preventDefault();
    const ref = pullRef.trim();
    if (!ref) return;
    const r = await runJob(`Pull ${ref}`, 'POST', '/images/pull', { image: ref });
    if (r.ok) pullRef = '';
  }

  async function remove(img) {
    const label = img.tags[0] || shortId(img.id);
    const ok = await confirm({
      title: `Remove ${label}?`,
      message: img.inUse
        ? `It's used by ${img.usedBy.join(', ')}. Remove those containers first, or force removal (running containers keep working until they're recreated).`
        : img.tags.length > 1 ? `This removes all its tags: ${img.tags.join(', ')}.` : 'You can pull it again later.',
      confirmLabel: img.inUse ? 'Force remove' : 'Remove',
      danger: true,
    });
    if (!ok) return;
    try {
      await api.del(`/images/${encodeURIComponent(img.id)}?force=${img.inUse || img.tags.length > 1 ? 1 : 0}`);
      toast(`Removed ${label}`, 'success', 2500);
      bumpLive();
    } catch (e) {
      toast(e.message, 'error', 7000);
    }
  }

  async function prune(all) {
    const ok = await confirm({
      title: all ? 'Remove all unused images?' : 'Remove dangling images?',
      message: all
        ? "Deletes every image that no container uses, including tagged ones. They'll be downloaded again if you need them."
        : 'Deletes untagged leftovers from old pulls and builds. This is always safe.',
      confirmLabel: 'Clean up',
      danger: all,
    });
    if (!ok) return;
    try {
      const r = await api.post('/images/prune', { all });
      toast(`Removed ${r.deleted} images, freed ${bytes(r.spaceReclaimed)}`, 'success');
      bumpLive();
    } catch (e) {
      toast(e.message, 'error');
    }
  }

  async function checkNow() {
    await api.post('/updates/check').catch((e) => toast(e.message, 'error'));
    toast('Checking registries for newer images…', 'info', 3000);
    updates = { ...(updates || {}), running: true };
    const poll = setInterval(async () => {
      const u = await api.get('/updates').catch(() => null);
      if (u && !u.running) {
        clearInterval(poll);
        updates = u;
        const n = u.results.filter((r) => r.available).length;
        toast(n ? `${n} images have updates` : 'Everything is up to date', n ? 'warn' : 'success');
        load();
      }
    }, 2000);
  }
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Images</h1>
      <p class="sub">{list ? `${list.length} images · ${bytes(total)}` : ' '}{unusedSize ? ` · ${bytes(unusedSize)} unused` : ''}</p>
    </div>
    <div class="row wrap">
      <button onclick={checkNow} disabled={updates?.running}>
        <Icon name="refresh" size={15} class={updates?.running ? 'spin' : ''} /> Check for updates
      </button>
      <Menu text="Clean up" icon="trash" items={[
        { label: 'Remove dangling images', hint: 'Untagged leftovers, always safe', onclick: () => prune(false) },
        { label: 'Remove all unused images', hint: 'Anything no container uses', danger: true, onclick: () => prune(true) },
      ]} />
    </div>
  </div>

  <form class="card pull" onsubmit={pull}>
    <Icon name="download" size={16} />
    <input bind:value={pullRef} placeholder="Pull an image, e.g. nginx:alpine or ghcr.io/owner/app:1.2" spellcheck="false" autocapitalize="off" />
    <button class="primary" type="submit" disabled={!pullRef.trim()}>Pull</button>
  </form>

  <div class="toolbar">
    <div class="search"><Icon name="search" size={15} /><input placeholder="Search images" bind:value={search} /></div>
    <div class="segmented">
      {#each [['all', 'All'], ['unused', 'Unused'], ['updates', 'Updates']] as [k, l]}
        <button class:active={filter === k} onclick={() => (filter = k)}>{l}</button>
      {/each}
    </div>
    {#if updates?.lastRun}<span class="dim small">Last checked {ago(updates.lastRun)}</span>{/if}
  </div>

  <div class="card">
    <div class="table-wrap">
      <table class="table">
        <thead><tr><th>Image</th><th class="hide-sm">ID</th><th>Size</th><th class="hide-sm">Created</th><th>Used by</th><th></th></tr></thead>
        <tbody>
          {#each shown as img (img.id)}
            <tr>
              <td>
                <div class="tags">
                  {#each img.tags as t}<span class="mono tag">{t}</span>{:else}<span class="dim">&lt;untagged&gt;</span>{/each}
                  {#if img.updateAvailable}<span class="badge amber tiny">Update available</span>{/if}
                </div>
              </td>
              <td class="hide-sm mono dim">{shortId(img.id)}</td>
              <td class="nowrap">{bytes(img.size)}</td>
              <td class="hide-sm nowrap dim">{ago(img.created)}</td>
              <td>
                {#if img.inUse}<span class="badge green" title={img.usedBy.join(', ')}>{img.inUse} container{img.inUse > 1 ? 's' : ''}</span>
                {:else}<span class="badge">Unused</span>{/if}
              </td>
              <td class="actions"><button class="ghost icon sm" title="Remove" aria-label="Remove" onclick={() => remove(img)}><Icon name="trash" size={15} /></button></td>
            </tr>
          {/each}
        </tbody>
      </table>
      {#if list && !shown.length}<div class="empty"><p>No images{list.length ? ' match' : ''}.</p></div>{/if}
    </div>
  </div>
</div>

<style>
  .pull { display: flex; align-items: center; gap: 10px; padding: 10px 10px 10px 16px; margin-bottom: 16px; }
  .pull > :global(svg) { color: var(--text-3); flex: none; }
  .pull input { border: 0; padding: 0; height: 34px; background: transparent; }
  .pull input:focus-visible { outline: none; }
  .toolbar { display: flex; gap: 10px; margin-bottom: 14px; flex-wrap: wrap; align-items: center; }
  .toolbar .search { flex: 1 1 240px; max-width: 360px; }
  .tags { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
  .tag { font-weight: 500; }
  .tiny { height: 19px; font-size: 11px; }
  .small { font-size: 12.5px; }
</style>
