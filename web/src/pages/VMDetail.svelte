<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import { bytes, percent, rate, bitrate } from '../lib/format.js';
  import { copy } from '../lib/clipboard.js';
  import Sparkline from '../components/Sparkline.svelte';
  import { vmStateLabel, vmStateColor, vmAction, setAutostart, deleteVM } from '../lib/vms.js';
  import { navigate } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';
  import VNCConsole from '../components/VNCConsole.svelte';
  import CodeEditor from '../components/CodeEditor.svelte';
  import VMSettings from '../components/VMSettings.svelte';
  import Terminal from '../components/Terminal.svelte';

  let { name, tab = 'overview' } = $props();

  let vm = $state(null);
  let notFound = $state(false);

  async function load() {
    try {
      vm = await api.get(`/vms/${encodeURIComponent(name)}`);
    } catch (e) {
      if (e.status === 404) notFound = true;
    }
  }
  // Charts (15 minutes, a point every 10 seconds), addresses and what the
  // guest agent reports.
  let m = $state(null);
  async function loadMetrics() {
    try {
      m = await api.get(`/vms/${encodeURIComponent(name)}/metrics`);
    } catch {
      // the VM page shows its own errors
    }
  }
  onMount(() => {
    load();
    loadMetrics();
    const t = setInterval(load, 3000);
    const t2 = setInterval(loadMetrics, 5000);
    return () => {
      clearInterval(t);
      clearInterval(t2);
    };
  });

  const on = $derived(vm?.state === 'running');
  const paused = $derived(vm?.state === 'paused');

  const hist = $derived(on ? m?.history || [] : []);
  const last = $derived(hist.at(-1));
  const series = (key) => hist.map((p) => p[key] || 0);
  const guest = $derived(on ? m?.guest : null);
  const diskStat = (path) => m?.disks.find((d) => d.path === path);
  const ifaceFor = (mac) => guest?.ifaces.find((i) => i.mac === mac?.toLowerCase());
  const otherIfaces = $derived((guest?.ifaces || []).filter((i) => !vm?.nics.some((n) => n.mac.toLowerCase() === i.mac)));
  const addrSource = { agent: 'reported by the VM', lease: "from libvirt's DHCP leases", arp: "from the server's ARP table" };
  const bare = (a) => a.split('/')[0];
  const pct = (a, b) => (b ? Math.min(100, (a / b) * 100) : 0);

  async function act(a) {
    if (await vmAction(vm, a)) setTimeout(load, 600);
  }
  async function toggleAuto() {
    if (await setAutostart(vm, !vm.autostart)) load();
  }

  const tabs = [
    { id: 'overview', label: 'Overview', icon: 'dashboard' },
    { id: 'settings', label: 'Settings', icon: 'settings' },
    { id: 'console', label: 'Console', icon: 'monitor' },
    { id: 'serial', label: 'Serial', icon: 'terminal' },
    { id: 'xml', label: 'XML', icon: 'code' },
  ];
  function goTab(t) {
    navigate(t === 'overview' ? `/vms/${encodeURIComponent(name)}` : `/vms/${encodeURIComponent(name)}/${t}`, { replace: true });
  }

  // ---- XML ----
  let xml = $state('');
  let savedXml = $state('');
  let xmlLoaded = $state(false);
  let saving = $state(false);
  const dirty = $derived(xmlLoaded && xml !== savedXml);
  $effect(() => {
    if (tab !== 'xml' || xmlLoaded) return;
    api.get(`/vms/${encodeURIComponent(name)}/xml`).then((r) => {
      xml = savedXml = r.xml;
      xmlLoaded = true;
    }).catch((e) => toast(e.message, 'error'));
  });
  async function saveXml() {
    saving = true;
    try {
      await api.put(`/vms/${encodeURIComponent(name)}/xml`, { xml });
      savedXml = xml;
      toast(on ? 'Saved. Changes apply the next time the VM starts.' : 'Saved', 'success', 4000);
      load();
    } catch (e) {
      toast(e.message, 'error', 10000);
    }
    saving = false;
  }

  // ---- Serial console ----
  let serialKey = $state(0); // bump to reconnect
  let serialForce = $state(false);
  let serialEnded = $state(''); // the close reason, once it ends
  let serialPending = $state(false); // added to the settings, waiting for a restart
  const serialReasons = {
    busy: 'someone else has this console open',
    off: 'the VM isn\'t running',
    noserial: 'no serial port',
    ended: 'disconnected',
  };
  $effect(() => {
    if (tab !== 'serial' || !vm || vm.serial) return;
    api.get(`/vms/${encodeURIComponent(name)}/config`).then((c) => (serialPending = !!c.serial)).catch(() => {});
  });
  function serialConnect(force = false) {
    serialForce = force;
    serialEnded = '';
    serialKey++;
  }
  async function addSerial() {
    try {
      const c = await api.get(`/vms/${encodeURIComponent(name)}/config`);
      await api.put(`/vms/${encodeURIComponent(name)}/config`, { ...c, serial: true });
      serialPending = true;
      toast(on || paused ? 'Serial port added. Shut the VM down and start it again to use it.' : 'Serial port added', 'success', 8000);
      load();
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
  }
</script>

<div class="page">
  <a class="back" href="/vms"><Icon name="arrow-left" size={14} /> Virtual machines</a>

  {#if notFound}
    <div class="card empty"><h3>VM not found</h3><p>There's no VM called {name}.</p><a class="btn" href="/vms">Back to VMs</a></div>
  {:else}
    <div class="page-header">
      <div>
        <div class="row wrap" style="gap:10px">
          <h1>{name}</h1>
          {#if vm}<span class="badge {vmStateColor(vm.state)}">{vmStateLabel[vm.state] || vm.state}</span>{/if}
        </div>
        {#if vm}<p class="sub">{vm.vcpus} vCPU · {bytes(vm.memory, 0)} · {vm.firmware === 'uefi' ? 'UEFI' : 'BIOS'} · {vm.machine}{#if on && vm.addrs?.length}{' · '}<button class="ip" title="Copy" onclick={() => copy(vm.addrs[0], `Copied ${vm.addrs[0]}`)}>{vm.addrs[0]}</button>{/if}</p>{/if}
      </div>
      {#if vm}
        <div class="row wrap">
          {#if on}
            <button onclick={() => act('shutdown')}><Icon name="power" size={15} /> Shut down</button>
            <button onclick={() => act('reboot')}><Icon name="restart" size={15} /> Reboot</button>
          {:else if paused}
            <button class="primary" onclick={() => act('resume')}><Icon name="play" size={15} /> Resume</button>
          {:else}
            <button class="primary" onclick={() => act('start')}><Icon name="play" size={15} /> Start</button>
          {/if}
          <Menu items={[
            on ? { label: 'Pause', icon: 'pause', onclick: () => act('pause') } : null,
            on ? { label: 'Reset', icon: 'restart', hint: 'Instant restart, no clean shutdown', onclick: () => act('reset') } : null,
            on || paused ? { label: 'Force off', icon: 'stop', hint: 'Like pulling the plug', onclick: () => act('stop') } : null,
            on || paused ? { divider: true } : null,
            { label: 'Delete VM', icon: 'trash', danger: true, onclick: () => deleteVM(vm) },
          ]} />
        </div>
      {/if}
    </div>

    <nav class="tabs">
      {#each tabs as t}
        <a href={t.id === 'overview' ? `/vms/${name}` : `/vms/${name}/${t.id}`} class:active={tab === t.id}
          onclick={(e) => { e.preventDefault(); goTab(t.id); }}><Icon name={t.icon} size={15} /> {t.label}{#if t.id === 'xml' && dirty}<span class="unsaved"></span>{/if}</a>
      {/each}
    </nav>

    {#if !vm}
      <div class="skeleton" style="height:260px"></div>
    {:else if tab === 'overview'}
      <div class="tiles">
        <div class="card tile">
          <div class="label"><Icon name="cpu" size={15} /> CPU</div>
          <div class="value">{on && last ? percent(last.cpu) : '–'}</div>
          <Sparkline series={[{ values: series('cpu'), color: 'var(--accent)' }]} max={100} label="CPU use, last 15 minutes" />
          <div class="foot">{vm.vcpus} vCPU{vm.vcpus > 1 ? 's' : ''}{on && !last ? ' · measuring…' : ''}</div>
        </div>
        <div class="card tile">
          <div class="label"><Icon name="memory" size={15} /> Memory</div>
          <div class="value">{on && last ? bytes(last.mem) : '–'} <small>of {bytes(vm.memory, 0)}</small></div>
          <Sparkline series={[{ values: series('mem'), color: 'var(--accent)' }]} max={vm.memory} label="Memory use, last 15 minutes" />
          <div class="foot" title={m?.guestMem ? "What programs in the VM use, as its memory driver reports (the VM's own cache isn't counted)" : 'How much of the server’s memory the VM has taken. The VM doesn’t report its own use, so this includes its caches.'}>{on ? (m?.guestMem ? 'Used by programs in the VM' : 'Taken from the server') : 'Not running'}</div>
        </div>
        <div class="card tile">
          <div class="label"><Icon name="network" size={15} /> Network</div>
          <div class="value two"><span><span class="arrow in">↓</span>{on && last ? bitrate(last.netRx) : '–'}</span><span><span class="arrow out">↑</span>{on && last ? bitrate(last.netTx) : '–'}</span></div>
          <Sparkline series={[{ values: series('netRx'), color: 'var(--accent)' }, { values: series('netTx'), color: 'var(--green)' }]} label="Network traffic, last 15 minutes" />
          <div class="foot">{vm.nics.length ? `${vm.nics.length} network card${vm.nics.length > 1 ? 's' : ''} · in and out` : 'No network card'}</div>
        </div>
        <div class="card tile">
          <div class="label"><Icon name="disk" size={15} /> Disk activity</div>
          <div class="value two"><span><span class="arrow in">R</span>{on && last ? rate(last.diskRead) : '–'}</span><span><span class="arrow wr">W</span>{on && last ? rate(last.diskWrite) : '–'}</span></div>
          <Sparkline series={[{ values: series('diskRead'), color: 'var(--accent)' }, { values: series('diskWrite'), color: 'var(--amber)' }]} label="Disk reads and writes, last 15 minutes" />
          <div class="foot">All disks · last 15 minutes</div>
        </div>
      </div>

      {#if on && vm.console === 'vnc'}
        <button class="card peek" onclick={() => goTab('console')}>
          <Icon name="terminal" size={18} /> <span>Open the console</span> <Icon name="chevron-right" size={16} />
        </button>
      {/if}

      <div class="cols">
        <section class="card sect">
          <div class="card-header"><h2>Inside the VM</h2>{#if guest?.info}<span class="dim small">From the QEMU guest agent</span>{/if}</div>
          {#if !on}
            <div class="li dim">Start the VM to see its operating system, addresses and disk space.</div>
          {:else if guest?.info}
            <dl class="kv">
              {#if guest.info.os}<dt>System</dt><dd>{guest.info.os}</dd>{/if}
              {#if guest.info.kernel}<dt>Kernel</dt><dd class="mono small">{guest.info.kernel}</dd>{/if}
              {#if guest.info.hostname}<dt>Hostname</dt><dd class="mono">{guest.info.hostname}</dd>{/if}
              {#if guest.info.timezone}<dt>Time zone</dt><dd>{guest.info.timezone}</dd>{/if}
            </dl>
            {#each guest.info.fs as f (f.mount)}
              <div class="fs">
                <span class="mono">{f.mount}</span>
                <div class="meter" class:amber={pct(f.used, f.total) >= 85} class:red={pct(f.used, f.total) >= 95}><div style="width:{pct(f.used, f.total)}%"></div></div>
                <span class="dim small nowrap">{bytes(f.used)} of {bytes(f.total)}</span>
              </div>
            {/each}
          {:else if vm.agent === 'connected'}
            <div class="li dim"><Icon name="loader" size={14} class="spin" /> Asking the guest agent…</div>
          {:else}
            <div class="agent-hint">
              <p>{vm.agent === '' ? 'This VM has no guest agent channel, so it can’t report anything from the inside.' : 'The QEMU guest agent isn’t running in the VM.'} With it, this shows the VM’s system, addresses and disk space, and Shut down works reliably.</p>
              {#if vm.agent === ''}
                <p class="dim small">Add a <code>&lt;channel type='unix'&gt;&lt;target type='virtio' name='org.qemu.guest_agent.0'/&gt;&lt;/channel&gt;</code> device in the XML tab (VMs made by Homestack have it), then install the agent.</p>
              {:else}
                <p class="dim small">Install it inside the VM: <code>sudo apt install qemu-guest-agent</code> (Debian, Ubuntu) or <code>sudo dnf install qemu-guest-agent</code> (Fedora, Alma, Rocky), then <code>sudo systemctl enable --now qemu-guest-agent</code>. On Windows it comes with the VirtIO drivers.</p>
              {/if}
            </div>
          {/if}
        </section>

        <section class="card sect">
          <div class="card-header"><h2>Details</h2></div>
          <dl class="kv">
            <dt>Addresses</dt>
            <dd>
              {#if on && vm.addrs?.length}
                <span class="ips">{#each vm.addrs as a}<button class="ip" title="Copy" onclick={() => copy(a, `Copied ${a}`)}>{a}</button>{/each}</span>
              {:else if on}<span class="dim">Not known yet{vm.agent !== 'connected' ? ' (the guest agent reports them)' : ''}</span>
              {:else}<span class="dim">Not running</span>{/if}
            </dd>
            <dt>Guest agent</dt>
            <dd>{#if vm.agent === 'connected'}<span class="badge green">Running</span>{:else if vm.agent && on}<span class="badge amber">Not running</span>{:else if vm.agent}<span class="dim">Set up</span>{:else}<span class="dim">No channel</span>{/if}</dd>
            {#if vm.template}<dt>Made from</dt><dd><a href="/vms/templates">{vm.template}</a></dd>{/if}
            <dt>Start with server</dt>
            <dd><button class="toggle" onclick={toggleAuto} aria-pressed={vm.autostart}><span class="switch" class:on={vm.autostart}></span>{vm.autostart ? 'On' : 'Off'}</button></dd>
          </dl>
        </section>
      </div>

      <section class="card sect">
        <div class="card-header"><h2>Disks</h2></div>
        {#each vm.disks as d}
          {@const ds = diskStat(d.path)}
          <div class="li wrap">
            <span class="badge">{d.device === 'cdrom' ? 'CD/DVD' : 'Disk'}</span>
            <span class="mono truncate grow path" title={d.path}>{d.path || '(empty)'}</span>
            {#if ds && d.device === 'disk'}<span class="small nowrap" title="Size the VM sees · space it takes on the server">{bytes(ds.capacity, 0)} · <span class="dim">{bytes(ds.allocation)} used</span></span>{/if}
            <span class="dim small nowrap">{d.target} · {d.bus}{d.format ? ` · ${d.format}` : ''}</span>
          </div>
        {:else}<div class="li dim">No disks</div>{/each}
      </section>

      <section class="card sect">
        <div class="card-header"><h2>Network</h2>{#if guest?.source && guest.source !== 'agent'}<span class="dim small">Addresses {addrSource[guest.source]}</span>{/if}</div>
        {#each vm.nics as n}
          {@const ifa = ifaceFor(n.mac)}
          <div class="li wrap">
            <Icon name="network" size={15} />
            <strong>{n.source || n.kind}</strong>
            {#if ifa?.addrs.length}<span class="ips">{#each ifa.addrs as a}<button class="ip" title="Copy" onclick={() => copy(bare(a), `Copied ${bare(a)}`)}>{a}</button>{/each}</span>{/if}
            <span class="grow"></span>
            {#if ifa && guest?.source === 'agent'}<span class="dim small" title="Its name inside the VM">{ifa.name}</span>{/if}
            <span class="mono dim small">{n.mac}</span>
            <span class="dim small">{n.model}</span>
          </div>
        {:else}<div class="li dim">No network</div>{/each}
        {#if otherIfaces.length}
          <div class="li wrap dim small">
            <span>Also inside the VM:</span>
            {#each otherIfaces as i}<span><span class="mono">{i.name}</span> {i.addrs.join(', ')}</span>{/each}
          </div>
        {/if}
      </section>

      {#if vm.passthrough}
        <p class="dim small">{vm.passthrough} passed-through device{vm.passthrough > 1 ? 's' : ''} (see the XML tab).</p>
      {/if}
      {#if vm.description}<p class="muted">{vm.description}</p>{/if}
    {:else if tab === 'settings'}
      <VMSettings {name} {vm} onsaved={() => { xmlLoaded = false; load(); }} />
    {:else if tab === 'console'}
      {#if on && vm.console === 'vnc'}
        <div class="console-box"><VNCConsole {name} /></div>
      {:else if on}
        <div class="card empty"><h3>No VNC console</h3><p>This VM uses {vm.console || 'no'} graphics. Add a VNC display in the XML tab to use the console here.</p></div>
      {:else}
        <div class="card empty">
          <h3>The VM isn't running</h3><p>Start it to see its screen.</p>
          <button class="primary" onclick={() => act('start')}><Icon name="play" size={15} /> Start</button>
        </div>
      {/if}
    {:else if tab === 'serial'}
      {#if !on && !paused}
        <div class="card empty">
          <h3>The VM isn't running</h3><p>Start it to use its serial console.</p>
          <button class="primary" onclick={() => act('start')}><Icon name="play" size={15} /> Start</button>
        </div>
      {:else if !vm.serial}
        <div class="card empty">
          {#if serialPending}
            <h3>Shut it down and start it again</h3>
            <p>The serial port is set up, but the VM only gets it the next time it starts (a reboot from inside isn't enough).</p>
          {:else}
            <h3>This VM has no serial port</h3>
            <p>A serial console is a plain text console that keeps working when the screen freezes, and you can copy and paste in it. Adding the port takes effect the next time the VM starts.</p>
            <button class="primary" onclick={addSerial}><Icon name="plus" size={15} /> Add a serial port</button>
          {/if}
        </div>
      {:else}
        <div class="xml-bar">
          <span class="dim small">The VM's text console, through its serial port: it works even when the screen is frozen. <strong>Nothing showing?</strong> Press Enter. Still nothing: the VM isn't offering a login there. On Debian or Ubuntu, run <code>sudo systemctl enable --now serial-getty@ttyS0.service</code> inside it. <kbd>Ctrl</kbd>+<kbd>]</kbd> disconnects.</span>
          <span class="spacer"></span>
          {#if serialEnded === 'busy'}
            <button class="primary" onclick={() => serialConnect(true)} title="Disconnects the other session (another tab, or virsh console over SSH)">Take over</button>
          {:else if serialEnded}
            <button onclick={() => serialConnect()}><Icon name="refresh" size={15} /> Reconnect</button>
          {/if}
        </div>
        {#if serialEnded === 'busy'}
          <div class="note"><Icon name="info" size={16} /><span>Someone else has this VM's serial console open: another browser tab, or <code>virsh console</code> over SSH. Only one session can use it at a time. <strong>Take over</strong> disconnects them.</span></div>
        {/if}
        {#key serialKey}
          <div class="console-box">
            <Terminal path="/api/vms/{encodeURIComponent(name)}/serial{serialForce ? '?force=1' : ''}" describe={(r) => serialReasons[r] || r || 'disconnected'} onclosed={(r) => (serialEnded = r || 'ended')} />
          </div>
        {/key}
      {/if}
    {:else if tab === 'xml'}
      <div class="xml-bar">
        <span class="dim small">The full libvirt definition, for what the Settings tab doesn't cover: CPU pinning, passthrough and more.</span>
        <span class="spacer"></span>
        {#if dirty}<button onclick={() => (xml = savedXml)}>Discard</button>{/if}
        <button class="primary" disabled={!dirty || saving} onclick={saveXml}><Icon name="save" size={15} /> Save</button>
      </div>
      {#if xmlLoaded}
        <CodeEditor bind:value={xml} language="xml" height="calc(100vh - 330px)" onsave={saveXml} />
      {:else}
        <div class="skeleton" style="height:400px"></div>
      {/if}
    {/if}
  {/if}
</div>

<style>
  .back { display: inline-flex; align-items: center; gap: 5px; color: var(--text-2); font-size: 13px; margin-bottom: 12px; }
  h1 { font-size: 24px; }
  .unsaved { width: 7px; height: 7px; border-radius: 50%; background: var(--amber); }
  .tiles { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 16px; }
  .tile { padding: 14px 16px 12px; display: grid; gap: 6px; align-content: start; }
  .label { display: flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); font-weight: 500; }
  .value { font-size: 22px; font-weight: 600; letter-spacing: -0.02em; font-variant-numeric: tabular-nums; white-space: nowrap; }
  .value small { font-size: 13px; font-weight: 400; color: var(--text-3); }
  .value.two { display: flex; gap: 14px; font-size: 17px; flex-wrap: wrap; }
  .arrow { font-size: 12px; font-weight: 600; margin-right: 4px; }
  .arrow.in { color: var(--accent); }
  .arrow.out { color: var(--green); }
  .arrow.wr { color: var(--amber); }
  .foot { font-size: 12px; color: var(--text-3); min-height: 17px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cols { display: grid; grid-template-columns: 3fr 2fr; gap: 16px; align-items: start; }
  .kv { display: grid; grid-template-columns: max-content 1fr; gap: 9px 16px; padding: 14px 18px; margin: 0; font-size: 13.5px; align-items: center; }
  .kv dt { color: var(--text-2); }
  .kv dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
  .fs { display: grid; grid-template-columns: minmax(70px, max-content) 1fr max-content; gap: 12px; align-items: center; padding: 9px 18px; border-top: 1px solid var(--border); font-size: 13px; }
  .agent-hint { padding: 14px 18px; display: grid; gap: 8px; font-size: 13.5px; line-height: 1.55; }
  .agent-hint p { margin: 0; }
  .ips { display: inline-flex; flex-wrap: wrap; gap: 6px; }
  .ip { all: unset; cursor: copy; font-family: var(--mono, ui-monospace, monospace); font-size: 12.5px; padding: 1px 7px; border-radius: 6px; background: var(--surface-2); color: var(--text); }
  .ip:hover { background: var(--accent-soft); color: var(--accent-text); }
  .ip:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  .sub .ip { font-size: 13px; }
  .toggle { all: unset; display: inline-flex; align-items: center; cursor: pointer; font-size: 13.5px; }
  .toggle:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 9px; }
  .li.wrap { flex-wrap: wrap; }
  .li.wrap .path { flex: 1 1 220px; } /* its own line on narrow screens, not "/s…" */
  .switch { width: 30px; height: 17px; border-radius: 9px; background: var(--border-strong); position: relative; margin-right: 8px; }
  .switch::after { content: ''; position: absolute; top: 2px; left: 2px; width: 13px; height: 13px; border-radius: 50%; background: #fff; transition: transform 0.15s; }
  .switch.on { background: var(--accent); }
  .switch.on::after { transform: translateX(13px); }
  .peek { all: unset; box-sizing: border-box; display: flex; gap: 10px; align-items: center; width: 100%; padding: 14px 18px; margin-bottom: 16px; cursor: pointer; background: var(--accent-soft); color: var(--accent-text); border-radius: 12px; font-weight: 500; }
  .peek span { flex: 1; }
  .sect { margin-bottom: 16px; }
  .li { display: flex; align-items: center; gap: 10px; padding: 10px 18px; border-bottom: 1px solid var(--border); font-size: 13.5px; min-width: 0; }
  .li:last-child { border-bottom: 0; }
  .grow { flex: 1; min-width: 0; }
  .small { font-size: 12.5px; }
  .console-box { height: calc(100vh - 290px); min-height: 420px; }
  .xml-bar { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; flex-wrap: wrap; }
  .spacer { flex: 1; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 13px; border-radius: 10px; margin-bottom: 12px; font-size: 13px; line-height: 1.5; background: var(--accent-soft); color: var(--accent-text); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  kbd { font-family: var(--mono); font-size: 11.5px; padding: 0 4px; border: 1px solid var(--border); border-radius: 4px; background: var(--surface-2); }
  @media (max-width: 1000px) { .cols { grid-template-columns: 1fr; } }
  @media (max-width: 860px) { .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
  @media (max-width: 480px) { .tiles { grid-template-columns: 1fr; } }
</style>
