<script>
  import { api } from '../lib/api.js';
  import { live, toast, confirm, bumpLive } from '../lib/state.svelte.js';
  import { ago } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';

  let list = $state(null);
  let search = $state('');
  let newName = $state('');
  let showAnon = $state(false);

  $effect(() => {
    live.tick;
    api.get('/volumes').then((l) => (list = l)).catch((e) => toast(e.message, 'error'));
  });

  const anonCount = $derived((list || []).filter((v) => v.anonymous).length);
  const shown = $derived(
    (list || []).filter((v) => (showAnon || !v.anonymous) && (!search || (v.name + (v.stack || '')).toLowerCase().includes(search.toLowerCase()))),
  );

  async function create(e) {
    e.preventDefault();
    try {
      await api.post('/volumes', { name: newName.trim() });
      toast(`Created ${newName}`, 'success', 2500);
      newName = '';
      bumpLive();
    } catch (err) {
      toast(err.message, 'error');
    }
  }

  async function remove(v) {
    const ok = await confirm({
      title: `Delete volume ${v.name}?`,
      message: v.usedBy.length
        ? `It's used by ${v.usedBy.join(', ')}. Remove those containers first.`
        : 'Everything stored in it is permanently deleted.',
      confirmLabel: 'Delete volume',
      danger: true,
      typeToConfirm: v.anonymous ? '' : v.name,
    });
    if (!ok) return;
    try {
      await api.del(`/volumes/${encodeURIComponent(v.name)}`);
      toast('Volume deleted', 'success', 2500);
      bumpLive();
    } catch (e) {
      toast(e.message, 'error', 7000);
    }
  }

  async function prune(all) {
    const ok = await confirm({
      title: all ? 'Delete all unused volumes?' : 'Delete unused anonymous volumes?',
      message: all
        ? 'Permanently deletes every volume no container is using, including named ones. Stopped stacks count as not using their volumes if their containers were removed.'
        : 'Deletes unnamed volumes left behind by removed containers.',
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) return;
    try {
      const r = await api.post('/volumes/prune', { all });
      toast(`Deleted ${r.deleted} volumes`, 'success');
      bumpLive();
    } catch (e) {
      toast(e.message, 'error');
    }
  }
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Volumes</h1>
      <p class="sub">Docker-managed storage. Folders bound from your stacks (like <code>./data</code>) live in the stack folder instead.</p>
    </div>
    <Menu text="Clean up" icon="trash" items={[
      { label: 'Delete unused anonymous volumes', hint: 'Unnamed leftovers', onclick: () => prune(false) },
      { label: 'Delete all unused volumes', hint: 'Including named ones', danger: true, onclick: () => prune(true) },
    ]} />
  </div>

  <div class="toolbar">
    <div class="search"><Icon name="search" size={15} /><input placeholder="Search volumes" bind:value={search} /></div>
    {#if anonCount}
      <label class="chk"><input type="checkbox" bind:checked={showAnon} /> Show {anonCount} anonymous</label>
    {/if}
    <span class="spacer"></span>
    <form class="row" onsubmit={create}>
      <input bind:value={newName} placeholder="New volume name" style="width:200px" />
      <button type="submit" disabled={!newName.trim()}><Icon name="plus" size={15} /> Create</button>
    </form>
  </div>

  <div class="card">
    <div class="table-wrap">
      <table class="table">
        <thead><tr><th>Name</th><th>Stack</th><th>Used by</th><th class="hide-sm">Driver</th><th class="hide-sm">Created</th><th></th></tr></thead>
        <tbody>
          {#each shown as v (v.name)}
            <tr>
              <td><span class="mono nm truncate" title={v.mountpoint}>{v.name}</span></td>
              <td>{#if v.stack}<a href="/stacks/{v.stack}">{v.stack}</a>{:else}<span class="dim">–</span>{/if}</td>
              <td>{#if v.usedBy.length}<span class="badge green" title={v.usedBy.join(', ')}>{v.usedBy.join(', ')}</span>{:else}<span class="badge">Unused</span>{/if}</td>
              <td class="hide-sm dim">{v.driver}</td>
              <td class="hide-sm nowrap dim">{ago(v.created)}</td>
              <td class="actions"><button class="ghost icon sm" title="Delete" aria-label="Delete" onclick={() => remove(v)}><Icon name="trash" size={15} /></button></td>
            </tr>
          {/each}
        </tbody>
      </table>
      {#if list && !shown.length}<div class="empty"><p>No volumes{list.length ? ' match' : ''}.</p></div>{/if}
    </div>
  </div>
</div>

<style>
  .toolbar { display: flex; gap: 10px; margin-bottom: 14px; flex-wrap: wrap; align-items: center; }
  .toolbar .search { flex: 1 1 220px; max-width: 320px; }
  .chk { display: flex; gap: 7px; align-items: center; font-size: 13px; color: var(--text-2); cursor: pointer; }
  .spacer { flex: 1; }
  .nm { display: inline-block; max-width: 340px; vertical-align: middle; }
</style>
