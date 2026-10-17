<script>
  import { rate, linkSpeed, dateTime } from '../../lib/format.js';
  import Icon from '../../lib/Icon.svelte';
  import Menu from '../Menu.svelte';
  import LinkEditor from './LinkEditor.svelte';
  import FileEditor from './FileEditor.svelte';

  // The server's own network: the ports, and the bonds, bridges and VLANs
  // built on them, with routing, the config files and backups.
  let { view, onreview } = $props();

  let showVirtual = $state(false);
  let editing = $state(null); // a link
  let file = $state(null); // { name } ('' for a new file)
  let allBackups = $state(false);

  const isVirtual = (l) => l.group || l.type === 'veth' || l.type === 'loopback';
  const byName = $derived(Object.fromEntries(view.links.map((l) => [l.name, l])));
  const visible = $derived(view.links.filter((l) => showVirtual || !isVirtual(l)));
  const hiddenCount = $derived(view.links.filter(isVirtual).length);
  const children = (name) => visible.filter((l) => l.master === name);
  const global = (l) => l.addrs.filter((a) => a.scope === 'global');
  const roots = $derived(
    visible
      .filter((l) => !l.master || !visible.includes(byName[l.master]))
      .sort((a, b) => (!!b.yours - !!a.yours) || (global(b).length > 0) - (global(a).length > 0) || a.name.localeCompare(b.name)),
  );
  // The newest few backups, and always the original.
  const backups = $derived.by(() => {
    if (allBackups) return view.backups;
    const rest = view.backups.filter((b) => b.id !== 'original');
    return [...rest.slice(0, 5), ...view.backups.filter((b) => b.id === 'original')];
  });
  const editable = (l) =>view.managed && l.config && l.networkFile && !l.networkFile.includes('/');

  function typeLabel(l) {
    switch (l.type) {
      case 'port': return `Network port${l.driver ? ` · ${l.driver}` : ''}`;
      case 'bond': return `Bond of ${view.links.filter((p) => p.master === l.name).length || 'its'} ports${l.bondMode ? ` · ${l.bondMode}` : ''}`;
      case 'bridge': {
        const v = view.links.find((p) => p.master === l.name && p.type === 'vlan');
        return v ? `Bridge for VLAN ${v.vlan}` : 'Bridge';
      }
      case 'vlan': return `VLAN ${l.vlan} on ${l.parent}`;
      case 'wireguard': return 'WireGuard VPN';
      case 'tunnel': return 'Tunnel';
      case 'veth': return 'Container link';
      case 'loopback': return 'Loopback (the server talking to itself)';
      case 'dummy': return 'Dummy';
      case 'macvlan': return 'macvlan';
      default: return l.type;
    }
  }
  function linkState(l) {
    if (!l.up) return ['Off', 'grey'];
    if (!l.carrier) return [l.type === 'port' ? 'No cable' : 'No link', 'amber'];
    return [l.speed ? `Up · ${linkSpeed(l.speed)}` : 'Up', 'green'];
  }
  function usage(l) {
    const u = view.usage?.[l.name];
    const out = [];
    if (u?.vms?.length) out.push(`VMs: ${u.vms.join(', ')}`);
    if (u?.docker?.length) out.push(`Docker: ${u.docker.join(', ')}`);
    return out.join(' · ');
  }
  function menu(l) {
    const items = [];
    if (editable(l)) items.push({ label: 'Edit settings', icon: 'settings', hint: 'Addresses, gateway, DNS, MTU', onclick: () => (editing = l) });
    if (l.networkFile && !l.networkFile.includes('/') && view.managed) items.push({ label: `Edit ${l.networkFile}`, icon: 'code', onclick: () => (file = { name: l.networkFile }) });
    if (l.netdevFile && view.managed) items.push({ label: `Edit ${l.netdevFile}`, icon: 'code', onclick: () => (file = { name: l.netdevFile }) });
    if (view.managed && !isVirtual(l)) {
      if (items.length) items.push({ divider: true });
      items.push(l.up
        ? { label: 'Take down', icon: 'stop', danger: true, hint: l.critical ? 'Your connection uses it' : 'Until you bring it back up or restart', disabled: l.critical, onclick: () => onreview({ op: 'link-state', link: l.name, up: false }) }
        : { label: 'Bring up', icon: 'play', onclick: () => onreview({ op: 'link-state', link: l.name, up: true }) });
    }
    return items;
  }

  // Routing
  const mainRoutes = $derived(
    view.routes
      .filter((r) => r.table === 'main' && (showVirtual || !isVirtual(byName[r.dev] || {})))
      .sort((a, b) => (b.dst === 'default') - (a.dst === 'default') || a.family - b.family || (a.metric || 0) - (b.metric || 0)),
  );
  const policyTables = $derived.by(() => {
    const t = {};
    for (const r of view.routes) if (r.table !== 'main' && r.table !== 'default') (t[r.table] ||= []).push(r);
    return Object.entries(t);
  });
  const ruleText = (r) =>
    [r.from && `from ${r.from}`, r.to && `to ${r.to}`, r.iif && `arriving on ${r.iif}`, r.oif && `leaving by ${r.oif}`].filter(Boolean).join(' ') || 'everything';
</script>

<div class="intro card">
  <Icon name="network" size={20} />
  <div>
    <p><strong>The server's own network connections.</strong> Physical ports, and the bonds, bridges and VLANs built on them. Docker and the VMs use these too, so a mistake here can cut everything off.</p>
    <p class="dim">That's why every change is shown to you first, tested (each connection must still reach its router), and <strong>undone automatically unless you keep it</strong>. The settings live in <code>{view.dir}</code>.</p>
  </div>
</div>

{#if !view.managed}
  <div class="note amber"><Icon name="alert" size={16} /><span>{view.note} Debian's other network tools (ifupdown, NetworkManager) aren't supported for changes.</span></div>
{/if}

<section class="card">
  <div class="card-header">
    <h2>Interfaces</h2>
    <span class="dim small">Indented ones are part of the one above (a bond's ports, a bridge's members).</span>
    <span class="spacer"></span>
    {#if hiddenCount}
      <label class="chk small"><input type="checkbox" bind:checked={showVirtual} /> Show {hiddenCount} Docker and VM interface{hiddenCount > 1 ? 's' : ''}</label>
    {/if}
  </div>
  {#snippet row(l, depth)}
    {@const [st, col] = linkState(l)}
    {@const rt = view.rates?.[l.name]}
    <div class="link" style="--depth:{depth}" class:yours={l.yours}>
      <span class="dot {col}" title={st}></span>
      <div class="main">
        <div class="line1">
          <strong class="mono">{l.name}</strong>
          <span class="dim small">{typeLabel(l)}</span>
          {#if l.portState === 'active' || l.portState === 'backup'}<span class="badge tiny" class:green={l.portState === 'active'}>{l.portState === 'active' ? 'Active port' : 'Standby'}</span>{/if}
          {#if l.yours}<span class="badge tiny accent" title="You reached Homestack on {view.via}">Your connection</span>
          {:else if l.critical}<span class="badge tiny" title="Your connection to Homestack runs through it">Your connection uses this</span>{/if}
          {#if l.setup === 'failed'}<span class="badge tiny red">networkd: failed</span>{/if}
        </div>
        <div class="line2">
          <span class="st {col}">{st}</span>
          {#each global(l) as a}<code class="addr">{a.addr}</code>{/each}
          {#if l.dns?.length}<span class="dim small">DNS {l.dns.join(', ')}</span>{/if}
          {#if usage(l)}<span class="dim small">{usage(l)}</span>{/if}
        </div>
      </div>
      <div class="rates small dim">
        {#if rt}<span title="Receiving">↓ {rate(rt[0])}</span><span title="Sending">↑ {rate(rt[1])}</span>{/if}
      </div>
      <div class="file small">
        {#if l.networkFile}
          {#if l.networkFile.includes('/')}<span class="dim" title="Not in {view.dir}: read-only here">{l.networkFile}</span>
          {:else}<button class="linkbtn" onclick={() => (file = { name: l.networkFile })} disabled={!view.managed}>{l.networkFile}</button>{/if}
        {:else if l.setup === 'unmanaged'}<span class="dim">not managed</span>{/if}
      </div>
      <div class="act">{#if menu(l).length}<Menu items={menu(l)} />{/if}</div>
    </div>
    {#each children(l.name) as c (c.name)}{@render row(c, depth + 1)}{/each}
  {/snippet}
  {#each roots as l (l.name)}{@render row(l, 0)}{/each}
</section>

<section class="card">
  <div class="card-header"><h2>Routing</h2><span class="dim small">Where traffic goes. The default route is the way out to the internet; if there are several, the lowest metric wins.</span></div>
  <div class="table-wrap">
    <table class="table">
      <thead><tr><th>Destination</th><th>Via</th><th>Interface</th><th class="hide-sm">Metric</th><th class="hide-sm">Set by</th></tr></thead>
      <tbody>
        {#each mainRoutes as r}
          <tr>
            <td>{#if r.dst === 'default'}<strong>Internet</strong> <span class="dim small">(default, IPv{r.family})</span>{:else}<code>{r.dst}</code>{/if}</td>
            <td>{#if r.gateway}<code>{r.gateway}</code>{:else}<span class="dim small">directly connected</span>{/if}</td>
            <td class="mono">{r.dev || '–'}</td>
            <td class="hide-sm dim">{r.metric ?? 0}</td>
            <td class="hide-sm dim small">{r.protocol === 'static' ? 'your settings' : r.protocol === 'kernel' ? 'automatic' : r.protocol || '–'}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  {#if view.rules.length || policyTables.length}
    <details class="policy">
      <summary><strong>Policy routing</strong> <span class="dim small">· {view.rules.length} rule{view.rules.length === 1 ? '' : 's'}, {policyTables.length} extra table{policyTables.length === 1 ? '' : 's'} (advanced)</span></summary>
      <p class="dim small">Rules pick a routing table by where traffic comes from. With two links on the same network, they make replies leave by the port the request arrived on. They're read-only here: change them in the files below.</p>
      <table class="table">
        <thead><tr><th>Order</th><th>Traffic</th><th>Uses table</th></tr></thead>
        <tbody>
          {#each view.rules as r}
            <tr><td class="dim">{r.priority}</td><td><code>{ruleText(r)}</code>{#if r.suppress != null} <span class="dim small">(only its specific routes, not its default)</span>{/if}</td><td>{r.table}</td></tr>
          {/each}
        </tbody>
      </table>
      {#each policyTables as [t, rs]}
        <h4>Table {t}</h4>
        <ul class="routes">{#each rs as r}<li><code>{r.dst === 'default' ? 'default' : r.dst}</code>{#if r.gateway} via <code>{r.gateway}</code>{/if} on <span class="mono">{r.dev}</span></li>{/each}</ul>
      {/each}
    </details>
  {/if}
</section>

<div class="two">
  <section class="card">
    <div class="card-header">
      <h2>Configuration files</h2>
      <span class="spacer"></span>
      {#if view.managed}<button class="sm" onclick={() => (file = { name: '' })}><Icon name="plus" size={14} /> New file</button>{/if}
    </div>
    <p class="dim small pad">Everything above comes from these files in <code>{view.dir}</code>. The forms edit them for you; open one directly for what the forms don't cover. It gets the same review, test and automatic undo.</p>
    <div class="list">
      {#each view.files as f (f.name)}
        <button class="frow" onclick={() => (file = { name: f.name })} disabled={!view.managed}>
          <Icon name="file" size={15} />
          <span class="mono grow">{f.name}</span>
          <span class="dim small">{f.links.length ? f.links.join(', ') : f.type === 'link' ? 'naming rules' : 'not in use'}</span>
        </button>
      {:else}<p class="pad dim">No files.</p>{/each}
    </div>
  </section>

  <section class="card">
    <div class="card-header"><h2>Backups</h2></div>
    <p class="dim small pad">A copy of the files is saved before every change. <strong>Original</strong> is how the network was before Homestack first changed it. Restoring goes through the same review and automatic undo.</p>
    <div class="list">
    {#each backups as b (b.id)}
      <div class="brow">
        <div class="grow">
          <div>{b.id === 'original' ? 'Original' : dateTime(b.time)}</div>
          <div class="dim small">{b.id === 'original' ? b.summary : `Saved before: ${b.summary.replace(/^Before: /, '')}`} · {b.files} file{b.files === 1 ? '' : 's'}</div>
        </div>
        {#if view.managed}<button class="sm" onclick={() => onreview({ op: 'restore', backup: b.id })}><Icon name="restart" size={14} /> Restore</button>{/if}
      </div>
    {:else}<p class="pad dim">None yet: the first is made with the first change.</p>{/each}
    </div>
    {#if view.backups.length > backups.length || allBackups}
      <button class="more" onclick={() => (allBackups = !allBackups)}>{allBackups ? 'Show fewer' : `Show all ${view.backups.length}`}</button>
    {/if}
  </section>
</div>

{#if editing}
  <LinkEditor link={editing} via={view.via} onclose={() => (editing = null)} onreview={(op) => { editing = null; onreview(op); }} />
{/if}
{#if file}
  <FileEditor name={file.name} dir={view.dir} onclose={() => (file = null)} onreview={(op) => { file = null; onreview(op); }} />
{/if}

<style>
  .intro { display: flex; gap: 14px; padding: 14px 18px; margin-bottom: 16px; align-items: flex-start; }
  .intro > :global(svg) { color: var(--accent); flex: none; margin-top: 2px; }
  .intro p { margin: 0 0 4px; font-size: 13.5px; line-height: 1.55; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 16px; font-size: 13.5px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  section.card { margin-bottom: 16px; }
  .spacer { flex: 1; }
  .small { font-size: 12.5px; }
  .chk { display: flex; gap: 6px; align-items: center; cursor: pointer; white-space: nowrap; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  .link { display: grid; grid-template-columns: 10px minmax(0, 1fr) 130px 190px 36px; gap: 12px; align-items: center; padding: 10px 18px 10px calc(18px + var(--depth) * 26px); border-bottom: 1px solid var(--border); position: relative; }
  .link:last-child { border-bottom: 0; }
  .link.yours { background: color-mix(in srgb, var(--accent-soft) 45%, transparent); }
  .dot { width: 9px; height: 9px; border-radius: 50%; background: var(--text-3); }
  .dot.green { background: var(--green); }
  .dot.amber { background: var(--amber); }
  .main { min-width: 0; display: grid; gap: 4px; }
  .line1, .line2 { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; min-width: 0; }
  .st { font-size: 12.5px; color: var(--text-2); }
  .st.amber { color: var(--amber-text); }
  .addr { font-size: 12px; }
  .tiny { height: 19px; font-size: 11px; }
  .rates { display: grid; gap: 2px; font-variant-numeric: tabular-nums; }
  .file { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .linkbtn { all: unset; cursor: pointer; color: var(--accent-text); font-family: var(--mono, ui-monospace, monospace); font-size: 12px; display: block; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .more { all: unset; cursor: pointer; display: block; padding: 10px 18px; font-size: 13px; color: var(--accent-text); border-top: 1px solid var(--border); }
  .linkbtn:hover { text-decoration: underline; }
  .policy { padding: 12px 18px; border-top: 1px solid var(--border); }
  .policy summary { cursor: pointer; }
  .policy p { margin: 8px 0; }
  .policy h4 { font-size: 13px; margin: 12px 0 4px; }
  .routes { margin: 0 0 0 18px; padding: 0; font-size: 13px; line-height: 1.8; }
  .two { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
  .pad { padding: 10px 18px 0; margin: 0; line-height: 1.5; }
  .frow { all: unset; box-sizing: border-box; display: flex; gap: 10px; align-items: center; width: 100%; padding: 9px 18px; border-bottom: 1px solid var(--border); cursor: pointer; font-size: 13px; }
  .frow:hover:not(:disabled) { background: var(--surface-2); }
  .frow:disabled { cursor: default; }
  .grow { flex: 1; min-width: 0; }
  .brow { display: flex; gap: 10px; align-items: center; padding: 9px 18px; border-bottom: 1px solid var(--border); font-size: 13px; }
  .list { margin-top: 10px; border-top: 1px solid var(--border); }
  .list > :last-child { border-bottom: 0; }
  @media (max-width: 1000px) {
    .two { grid-template-columns: 1fr; }
    .link { grid-template-columns: 10px minmax(0, 1fr) 36px; }
    .rates, .file { display: none; }
  }
</style>
