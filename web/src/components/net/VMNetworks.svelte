<script>
  import { onMount } from 'svelte';
  import { api } from '../../lib/api.js';
  import { toast, confirm } from '../../lib/state.svelte.js';
  import Icon from '../../lib/Icon.svelte';
  import Modal from '../Modal.svelte';

  // How VMs reach the network: host bridges (a VM sits on the LAN or a
  // VLAN like any other machine) and libvirt's private virtual networks.
  let { host = null, hostError = '', onreview } = $props();

  let data = $state(null);
  async function load() {
    try {
      data = await api.get('/network/vms');
    } catch (e) {
      data = { available: false, error: e.message };
    }
  }
  onMount(() => {
    load();
    const t = setInterval(load, 6000);
    return () => clearInterval(t);
  });

  // ---- Bridges ----
  const hostLinks = $derived(host?.links || []);
  const byName = $derived(Object.fromEntries(hostLinks.map((l) => [l.name, l])));
  const bridges = $derived.by(() => {
    const names = new Set([...(data?.bridges || []).map((b) => b.name), ...hostLinks.filter((l) => l.type === 'bridge' && !l.group).map((l) => l.name)]);
    return [...names].sort().map((name) => {
      const l = byName[name];
      const ports = hostLinks.filter((p) => p.master === name && !p.group);
      const vlan = ports.find((p) => p.type === 'vlan');
      const addrs = (l?.addrs || []).filter((a) => a.scope === 'global');
      return { name, link: l, vlan, ports, addrs, vms: data?.bridges?.find((b) => b.name === name)?.vms || [] };
    });
  });
  const canChangeHost = $derived(!!host?.managed);

  function describe(b) {
    if (b.vlan) return `VLAN ${b.vlan.vlan}, carried on ${b.vlan.parent}`;
    if (b.ports.length) return `Your LAN, through ${b.ports.map((p) => p.name).join(', ')}`;
    return b.link ? 'No ports: VMs on it only reach each other and the server' : '';
  }

  let wizard = $state(null);
  const parents = $derived(hostLinks.filter((l) => (l.type === 'port' || l.type === 'bond') && !l.group && l.networkFile && !l.networkFile.includes('/')));
  function openWizard() {
    // Default to the link under the main bridge (bond0 under br0, say).
    const under = parents.find((p) => p.master && byName[p.master]?.addrs?.some((a) => a.scope === 'global')) || parents[0];
    wizard = { parent: under?.name || '', vlan: '', bridge: '', address: '' };
  }
  const suggested = $derived.by(() => {
    if (!wizard?.parent || !wizard.vlan) return '';
    const p = byName[wizard.parent];
    const b = p?.master ? `${p.master}.${wizard.vlan}` : `brvlan${wizard.vlan}`;
    return b.length <= 15 ? b : `brvlan${wizard.vlan}`;
  });
  function reviewVLAN(e) {
    e.preventDefault();
    const w = wizard;
    wizard = null;
    onreview({ op: 'vlan-add', parent: w.parent, vlan: +w.vlan, bridge: w.bridge.trim() || suggested, address: w.address.trim() });
  }

  // ---- libvirt networks ----
  let creating = $state(null);
  let error = $state('');
  let busy = $state('');
  function openCreate() {
    error = '';
    creating = { name: '', mode: 'nat', subnet: data?.suggest || '192.168.100.0/24', dhcp: true, autostart: true };
  }
  async function create(e) {
    e.preventDefault();
    error = '';
    busy = 'create';
    try {
      await api.post('/network/vms', creating);
      toast(`Created ${creating.name}`, 'success');
      creating = null;
      load();
    } catch (err) {
      error = err.message;
    }
    busy = '';
  }
  async function action(n, a) {
    busy = n.name;
    try {
      await api.post(`/network/vms/${encodeURIComponent(n.name)}/action`, { action: a });
      load();
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    busy = '';
  }
  async function remove(n) {
    const ok = await confirm({
      title: `Delete the network ${n.name}?`,
      message: n.vms.length ? `${n.vms.map((v) => v.name).join(', ')} still use it: move them to another network first.` : 'No VMs use it. This stops it and removes its definition.',
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) return;
    busy = n.name;
    try {
      await api.del(`/network/vms/${encodeURIComponent(n.name)}`);
      toast(`Deleted ${n.name}`, 'success');
      load();
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    busy = '';
  }
  const modeLabel = { nat: 'NAT: internet through the server', isolated: 'Isolated: VMs and the server only', route: 'Routed', open: 'Open', bridge: 'Uses a host bridge' };
  const leaseFor = (n, mac) => n.leases.find((l) => l.mac === mac)?.ip;
</script>

<div class="intro card">
  <Icon name="vm" size={20} />
  <div>
    <p><strong>How VMs connect.</strong> Pick one of these in a VM's Settings → Network.</p>
    <ul>
      <li><strong>Bridge:</strong> the VM sits on your LAN (or a VLAN) like any other machine, and gets its address from your router. The usual choice.</li>
      <li><strong>Virtual network:</strong> a private network inside this server. libvirt gives VMs their addresses, and either shares the server's connection (NAT) or keeps them to themselves (isolated).</li>
    </ul>
  </div>
</div>

<section class="card">
  <div class="card-header">
    <h2>Bridges</h2>
    <span class="spacer"></span>
    {#if canChangeHost}<button class="sm" onclick={openWizard}><Icon name="plus" size={14} /> Add a VLAN bridge</button>{/if}
  </div>
  {#if hostError && !host}<p class="pad dim small">{hostError} Bridges are listed, but adding or removing VLANs needs the host service.</p>{/if}
  <div class="list">
    {#each bridges as b (b.name)}
      <div class="brow">
        <div class="grow">
          <div class="line1"><strong class="mono">{b.name}</strong> <span class="dim small">{describe(b)}</span></div>
          <div class="line2 small">
            {#if b.addrs.length}{#each b.addrs as a}<code>{a.addr}</code>{/each}
            {:else if b.vlan}<span class="dim">No server address: only VMs use it</span>{/if}
            {#if b.vms.length}
              <span class="dim">VMs:</span>{#each b.vms as v}<a href="/vms/{encodeURIComponent(v.name)}" class="vm"><span class="d" class:on={v.state === 'running'}></span>{v.name}</a>{/each}
            {:else}<span class="dim">No VMs</span>{/if}
          </div>
        </div>
        {#if canChangeHost && b.vlan}
          <button class="sm" disabled={b.vms.length > 0 || b.link?.critical} title={b.vms.length ? 'VMs use it: move them first' : ''}
            onclick={() => onreview({ op: 'vlan-remove', bridge: b.name })}><Icon name="trash" size={14} /> Remove</button>
        {/if}
      </div>
    {:else}<p class="pad dim">No bridges on this server.</p>{/each}
  </div>
</section>

<section class="card">
  <div class="card-header">
    <h2>Virtual networks</h2>
    <span class="dim small">libvirt's private networks</span>
    <span class="spacer"></span>
    {#if data?.available}<button class="sm" onclick={openCreate}><Icon name="plus" size={14} /> New virtual network</button>{/if}
  </div>
  {#if !data}
    <div class="skeleton" style="height:90px;margin:14px"></div>
  {:else if !data.available}
    <p class="pad dim">{data.error || 'libvirt isn\'t available.'}</p>
  {:else}
    <div class="list">
      {#each data.networks as n (n.name)}
        <div class="brow">
          <div class="grow">
            <div class="line1">
              <strong class="mono">{n.name}</strong>
              <span class="badge tiny" class:green={n.active}>{n.active ? 'Running' : 'Stopped'}</span>
              <span class="dim small">{modeLabel[n.mode] || n.mode}</span>
            </div>
            <div class="line2 small">
              {#if n.address}<code>{n.address}</code>{/if}
              {#if n.dhcpStart}<span class="dim">hands out {n.dhcpStart} – {n.dhcpEnd}</span>{:else if n.address}<span class="dim">no DHCP: set addresses in the VMs</span>{/if}
              {#if n.bridge}<span class="dim">on {n.bridge}</span>{/if}
            </div>
            <div class="line2 small">
              {#if n.vms.length}
                <span class="dim">VMs:</span>{#each n.vms as v}<a href="/vms/{encodeURIComponent(v.name)}" class="vm"><span class="d" class:on={v.state === 'running'}></span>{v.name}{#if leaseFor(n, v.mac)} <code>{leaseFor(n, v.mac)}</code>{/if}</a>{/each}
              {:else}<span class="dim">No VMs</span>{/if}
            </div>
          </div>
          <label class="auto small" title="Start it when the server starts"><input type="checkbox" checked={n.autostart} disabled={busy === n.name} onchange={() => action(n, n.autostart ? 'no-autostart' : 'autostart')} /> Start with server</label>
          {#if n.active}
            <button class="sm" disabled={busy === n.name} onclick={() => action(n, 'stop')}><Icon name="stop" size={14} /> Stop</button>
          {:else}
            <button class="sm" disabled={busy === n.name} onclick={() => action(n, 'start')}><Icon name="play" size={14} /> Start</button>
          {/if}
          <button class="ghost icon sm" title="Delete" aria-label="Delete" disabled={busy === n.name} onclick={() => remove(n)}><Icon name="trash" size={15} /></button>
        </div>
      {:else}<p class="pad dim">No virtual networks. VMs can use a bridge instead.</p>{/each}
    </div>
  {/if}
</section>

{#if wizard}
  <Modal title="Add a VLAN bridge for VMs" onclose={() => (wizard = null)} width={560}>
    <form id="vlanform" class="stack-v" onsubmit={reviewVLAN}>
      <p class="hint">A VLAN splits one cable into separate networks, so VMs can be kept apart from the rest of your LAN (a lab, guests, cameras). This adds the VLAN on the server and a bridge for VMs to join. Your switch must send the VLAN to this server <strong>tagged</strong> on the same port.</p>
      <label class="field"><span>Carried on</span>
        <select bind:value={wizard.parent} required>
          {#each parents as p}<option value={p.name}>{p.name} ({p.type === 'bond' ? 'bond' : 'port'}{p.master ? `, part of ${p.master}` : ''})</option>{/each}
        </select>
        <span class="hint">The port or bond plugged into the switch that carries the VLAN.</span></label>
      <label class="field"><span>VLAN ID</span>
        <input type="number" min="1" max="4094" bind:value={wizard.vlan} required placeholder="50" style="max-width:140px" />
        <span class="hint">The number your switch and router use for it, 1 to 4094.</span></label>
      <label class="field"><span>Bridge name</span>
        <input class="mono" bind:value={wizard.bridge} placeholder={suggested || 'br0.50'} autocapitalize="off" spellcheck="false" />
        <span class="hint">What VMs pick in Settings → Network. Leave empty for {suggested || 'the usual name'}.</span></label>
      <label class="field"><span>Server address on this VLAN (optional)</span>
        <input class="mono" bind:value={wizard.address} placeholder="none" autocapitalize="off" spellcheck="false" />
        <span class="hint">Usually left empty: VMs use the VLAN, and the server stays off it (and unreachable from it). Give it one, like <code>10.0.50.5/24</code>, only if the server should be on that network too.</span></label>
    </form>
    {#snippet footer()}
      <button onclick={() => (wizard = null)}>Cancel</button>
      <button class="primary" type="submit" form="vlanform" disabled={!wizard.parent || !wizard.vlan}><Icon name="eye" size={15} /> Review the change</button>
    {/snippet}
  </Modal>
{/if}

{#if creating}
  <Modal title="New virtual network" onclose={() => (creating = null)} width={560}>
    <form id="vnetform" class="stack-v" onsubmit={create}>
      <label class="field"><span>Name</span>
        <!-- svelte-ignore a11y_autofocus -->
        <input bind:value={creating.name} placeholder="lab" required pattern="[A-Za-z0-9][A-Za-z0-9_.\-]*" autofocus autocapitalize="off" spellcheck="false" /></label>
      <div class="field"><span>Kind</span>
        <div class="segmented">
          <button type="button" class:active={creating.mode === 'nat'} onclick={() => (creating.mode = 'nat')}>NAT</button>
          <button type="button" class:active={creating.mode === 'isolated'} onclick={() => (creating.mode = 'isolated')}>Isolated</button>
        </div>
        <span class="hint">{creating.mode === 'nat' ? 'VMs reach the internet and your LAN through the server\'s address, but nothing on the LAN can start a connection to them. Good for a private lab with internet.' : 'VMs reach each other and the server, nothing else. Good for testing something you don\'t trust.'}</span>
      </div>
      <label class="field"><span>Network</span>
        <input class="mono" bind:value={creating.subnet} required autocapitalize="off" spellcheck="false" />
        <span class="hint">A private range nothing else uses (checked for you). The server takes the first address, {creating.subnet.split('/')[0].replace(/\.0$/, '.1')} here.</span></label>
      <label class="chk"><input type="checkbox" bind:checked={creating.dhcp} /> <span>Hand out addresses to VMs (DHCP)</span></label>
      <label class="chk"><input type="checkbox" bind:checked={creating.autostart} /> <span>Start it when the server starts</span></label>
      {#if error}<p class="error-text">{error}</p>{/if}
    </form>
    {#snippet footer()}
      <button onclick={() => (creating = null)}>Cancel</button>
      <button class="primary" type="submit" form="vnetform" disabled={busy === 'create'}>Create</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .intro { display: flex; gap: 14px; padding: 14px 18px; margin-bottom: 16px; align-items: flex-start; }
  .intro > :global(svg) { color: var(--accent); flex: none; margin-top: 2px; }
  .intro p { margin: 0 0 4px; font-size: 13.5px; }
  .intro ul { margin: 4px 0 0 18px; padding: 0; font-size: 13.5px; line-height: 1.6; }
  section.card { margin-bottom: 16px; }
  .spacer { flex: 1; }
  .small { font-size: 12.5px; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  .pad { padding: 12px 18px; margin: 0; }
  .list > :last-child { border-bottom: 0; }
  .brow { display: flex; gap: 12px; align-items: center; padding: 11px 18px; border-bottom: 1px solid var(--border); flex-wrap: wrap; }
  .grow { flex: 1; min-width: 220px; display: grid; gap: 4px; }
  .line1, .line2 { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .tiny { height: 19px; font-size: 11px; }
  .vm { display: inline-flex; gap: 5px; align-items: center; }
  .d { width: 7px; height: 7px; border-radius: 50%; background: var(--text-3); }
  .d.on { background: var(--green); }
  .auto { display: flex; gap: 6px; align-items: center; cursor: pointer; color: var(--text-2); }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .chk { display: flex; gap: 8px; align-items: center; font-size: 13.5px; cursor: pointer; }
</style>
