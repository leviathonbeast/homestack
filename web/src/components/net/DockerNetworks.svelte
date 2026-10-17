<script>
  import { api } from '../../lib/api.js';
  import { live, toast, confirm, bumpLive } from '../../lib/state.svelte.js';
  import Icon from '../../lib/Icon.svelte';
  import Modal from '../Modal.svelte';

  // Docker's networks: private ones that connect containers to each other,
  // and LAN ones (macvlan / ipvlan) that give containers their own address
  // on your network.
  let list = $state(null);
  let parents = $state([]);
  let creating = $state(null);
  let error = $state('');
  let busy = $state(false);

  $effect(() => {
    live.tick;
    api.get('/networks').then((l) => (list = l)).catch((e) => toast(e.message, 'error'));
  });

  const kinds = [
    { id: 'bridge', label: 'Private', icon: 'layers', text: 'Containers on it reach each other by name, and the internet through the server. Use it to connect stacks, like apps behind a reverse proxy.' },
    { id: 'macvlan', label: 'On your LAN', icon: 'network', text: 'Each container gets its own address on your network, like a separate machine. Note: the server itself can\'t reach these containers directly (a Linux limitation).' },
    { id: 'ipvlan', label: 'On your LAN (shared MAC)', icon: 'plug', text: 'Like "On your LAN", but containers share the server\'s MAC address. Use it if your router or switch only allows a few devices per port.' },
  ];
  const driverLabel = (d) => ({ bridge: 'Private', macvlan: 'LAN (macvlan)', ipvlan: 'LAN (ipvlan)', host: 'Host', null: 'None' })[d] || d;

  async function open() {
    error = '';
    creating = { name: '', driver: 'bridge', internal: false, parent: '', subnet: '', gateway: '', ipRange: '' };
    try {
      parents = await api.get('/networks/parents');
    } catch {
      parents = [];
    }
  }
  function pickParent(name) {
    const p = parents.find((x) => x.name === name);
    creating.parent = name;
    if (p) {
      creating.subnet = p.subnet || '';
      creating.gateway = p.gateway || '';
      creating.ipRange = p.ipRange || '';
    }
  }
  $effect(() => {
    // Choosing a LAN kind picks the first free interface for you.
    if (creating && creating.driver !== 'bridge' && !creating.parent && parents.length) {
      const free = parents.find((p) => !p.docker.length && p.addrs.length) || parents.find((p) => !p.docker.length);
      if (free) pickParent(free.name);
    }
  });
  const lan = $derived(creating && creating.driver !== 'bridge');
  const chosen = $derived(parents.find((p) => p.name === creating?.parent));

  async function create(e) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      await api.post('/networks', { ...creating, ...(lan ? {} : { parent: '', gateway: '', ipRange: '' }) });
      toast(`Created ${creating.name}`, 'success', 2500);
      creating = null;
      bumpLive();
    } catch (err) {
      error = err.message;
    }
    busy = false;
  }

  async function remove(n) {
    const ok = await confirm({
      title: `Remove network ${n.name}?`,
      message: n.containers.length ? `Containers still connected: ${n.containers.join(', ')}. Disconnect or stop them first.` : 'No containers are using it.',
      confirmLabel: 'Remove',
      danger: true,
    });
    if (!ok) return;
    try {
      await api.del(`/networks/${n.id}`);
      toast('Network removed', 'success', 2500);
      bumpLive();
    } catch (e) {
      toast(e.message, 'error', 7000);
    }
  }

  async function prune() {
    const ok = await confirm({ title: 'Remove unused networks?', message: 'Removes every custom network with no containers connected.', confirmLabel: 'Remove' });
    if (!ok) return;
    try {
      const r = await api.post('/networks/prune');
      toast(`Removed ${r.deleted} networks`, 'success');
      bumpLive();
    } catch (e) {
      toast(e.message, 'error');
    }
  }
</script>

<div class="intro card">
  <Icon name="layers" size={20} />
  <div class="grow">
    <p><strong>Networks for containers.</strong> Containers on the same network reach each other by name. <em>Private</em> networks stay inside the server; <em>LAN</em> networks give containers their own address on your network.</p>
  </div>
  <button onclick={prune}><Icon name="trash" size={15} /> Remove unused</button>
  <button class="primary" onclick={open}><Icon name="plus" size={15} /> New network</button>
</div>

<div class="card">
  <div class="table-wrap">
    <table class="table">
      <thead><tr><th>Name</th><th>Kind</th><th class="hide-sm">Addresses</th><th>Containers</th><th class="hide-sm">Stack</th><th></th></tr></thead>
      <tbody>
        {#each list || [] as n (n.id)}
          <tr>
            <td><strong>{n.name}</strong>{#if n.system}<span class="badge tiny">Built in</span>{/if}{#if n.internal}<span class="badge tiny">No internet</span>{/if}</td>
            <td class="dim">{driverLabel(n.driver)}{#if n.parent} <span class="small">on <span class="mono">{n.parent}</span></span>{/if}</td>
            <td class="hide-sm mono dim small">{n.subnet || '–'}{#if n.ipRange}<br />range {n.ipRange}{/if}</td>
            <td>
              {#if n.containers.length}
                <span class="small">{#each n.containers as c, i}{i ? ', ' : ''}{c}{#if n.ips[c] && n.driver !== 'bridge'} <code>{n.ips[c]}</code>{/if}{/each}</span>
              {:else}<span class="dim">None</span>{/if}
            </td>
            <td class="hide-sm">{#if n.stack}<a href="/stacks/{n.stack}">{n.stack}</a>{:else}<span class="dim">–</span>{/if}</td>
            <td class="actions">
              {#if !n.system}<button class="ghost icon sm" title="Remove" aria-label="Remove" onclick={() => remove(n)}><Icon name="trash" size={15} /></button>{/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
</div>

<p class="dim tip">To use a shared network from a stack, add it to the compose file:<br />
  <code>networks: {'{'} proxy: {'{'} external: true {'}'} {'}'}</code>, then list it under each service's <code>networks:</code>.
  On a LAN network you can pick a container's address with <code>ipv4_address:</code>.</p>

{#if creating}
  <Modal title="New Docker network" onclose={() => (creating = null)} width={600}>
    <form id="netform" class="stack-v" onsubmit={create}>
      <label class="field"><span>Name</span>
        <!-- svelte-ignore a11y_autofocus -->
        <input bind:value={creating.name} placeholder={lan ? 'lan' : 'proxy'} autofocus required autocapitalize="off" spellcheck="false" /></label>
      <div class="field"><span>Kind</span>
        <div class="kinds">
          {#each kinds as k}
            <button type="button" class="kind" class:active={creating.driver === k.id} onclick={() => (creating.driver = k.id)}>
              <Icon name={k.icon} size={16} /><strong>{k.label}</strong><small>{k.text}</small>
            </button>
          {/each}
        </div>
      </div>

      {#if lan}
        <label class="field"><span>Joins</span>
          <select value={creating.parent} onchange={(e) => pickParent(e.currentTarget.value)} required>
            <option value="" disabled>Choose an interface</option>
            {#each parents as p}
              <option value={p.name} disabled={p.docker.length > 0}>{p.name} ({p.addrs[0] || (p.type === 'bridge' ? 'no address: a VMs-only VLAN bridge?' : p.type)}){p.docker.length ? ` · used by ${p.docker.join(', ')}` : ''}</option>
            {/each}
          </select>
          <span class="hint">The server interface on the network the containers should be on: your LAN bridge (like br0), or a VLAN bridge to put them on that VLAN.</span></label>
        <div class="grid3">
          <label class="field"><span>Network</span><input class="mono" bind:value={creating.subnet} placeholder="10.0.10.0/24" required autocapitalize="off" spellcheck="false" /></label>
          <label class="field"><span>Router (gateway)</span><input class="mono" bind:value={creating.gateway} placeholder="10.0.10.1" required autocapitalize="off" spellcheck="false" /></label>
          <label class="field"><span>Addresses for containers</span><input class="mono" bind:value={creating.ipRange} placeholder="10.0.10.224/27" required autocapitalize="off" spellcheck="false" /></label>
        </div>
        <div class="note"><Icon name="alert" size={16} /><span>Docker gives containers addresses from <strong>{creating.ipRange || 'the range'}</strong> without asking your router. Keep that range out of your router's DHCP pool (or reserve it), or two devices could end up with the same address.{#if chosen && !chosen.addrs.length} {chosen.name} has no address of its own, so enter that VLAN's network and router.{/if}</span></div>
      {:else}
        <label class="field"><span>Network (optional)</span>
          <input class="mono" bind:value={creating.subnet} placeholder="Docker picks a free one" autocapitalize="off" spellcheck="false" />
          <span class="hint">Only set it if you need fixed addresses. It's checked against your LAN, so containers can still reach it.</span></label>
        <label class="row chk"><input type="checkbox" bind:checked={creating.internal} /> No internet access (containers only reach each other)</label>
      {/if}
      {#if error}<p class="error-text">{error}</p>{/if}
    </form>
    {#snippet footer()}
      <button onclick={() => (creating = null)}>Cancel</button>
      <button class="primary" type="submit" form="netform" disabled={busy}>Create</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .intro { display: flex; gap: 12px; padding: 14px 18px; margin-bottom: 16px; align-items: center; flex-wrap: wrap; }
  .intro > :global(svg) { color: var(--accent); flex: none; }
  .intro p { margin: 0; font-size: 13.5px; line-height: 1.55; }
  .grow { flex: 1; min-width: 260px; }
  .tiny { height: 19px; font-size: 11px; margin-left: 8px; }
  .small { font-size: 12.5px; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  .tip { margin-top: 16px; font-size: 13px; line-height: 1.8; }
  .chk { font-size: 13.5px; color: var(--text); cursor: pointer; display: flex; gap: 8px; align-items: center; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .kinds { display: grid; gap: 8px; }
  .kind { all: unset; box-sizing: border-box; display: grid; grid-template-columns: 22px 1fr; column-gap: 8px; row-gap: 2px; padding: 10px 12px; border: 1px solid var(--border); border-radius: 10px; cursor: pointer; }
  .kind :global(svg) { grid-row: span 2; margin-top: 2px; color: var(--text-3); }
  .kind strong { font-size: 13.5px; font-weight: 500; }
  .kind small { font-size: 12.5px; color: var(--text-3); line-height: 1.45; }
  .kind:hover { border-color: var(--border-strong); }
  .kind.active { border-color: var(--accent); background: var(--accent-soft); }
  .kind.active :global(svg) { color: var(--accent); }
  .grid3 { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 13px; border-radius: 10px; font-size: 13px; line-height: 1.5; background: var(--amber-soft); color: var(--amber-text); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  @media (max-width: 640px) { .grid3 { grid-template-columns: 1fr; } }
</style>
