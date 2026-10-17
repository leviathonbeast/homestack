<script>
  // The VM settings form: CPUs, memory, disks and CDs, network, display, USB
  // devices and boot order, without editing XML. Anything else in the
  // definition (passthrough, CPU pinning…) is kept as it is.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast, confirm } from '../lib/state.svelte.js';
  import { bytes } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';

  let { name, vm, onsaved } = $props();

  let cfg = $state(null);
  let orig = $state('');
  let opts = $state(null);
  let error = $state('');
  let saving = $state(false);

  async function load() {
    try {
      const [c, o] = await Promise.all([api.get(`/vms/${encodeURIComponent(name)}/config`), opts ? opts : api.get('/vms/options')]);
      cfg = c;
      orig = JSON.stringify(c);
      opts = o;
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(load);

  const dirty = $derived(!!cfg && JSON.stringify(cfg) !== orig);
  const running = $derived(vm?.state === 'running' || vm?.state === 'paused');
  const maxMemGB = $derived(opts ? Math.max(1, Math.floor(opts.hostMemory / 1024 ** 3)) : 64);
  const nicModels = ['virtio', 'e1000e', 'e1000', 'rtl8139', 'vmxnet3'];
  const videos = [['virtio', 'Virtio (Linux)'], ['vga', 'VGA'], ['qxl', 'QXL (SPICE drivers)'], ['bochs', 'Bochs'], ['cirrus', 'Cirrus (very old OSes)'], ['none', 'None']];
  const busLabel = { virtio: 'VirtIO (fastest)', sata: 'SATA', scsi: 'SCSI (virtio-scsi)', usb: 'USB', ide: 'IDE' };

  const getMemGB = () => (cfg ? +(cfg.memoryMB / 1024).toFixed(2) : 0);
  const setMemGB = (v) => (cfg.memoryMB = Math.round(+v * 1024));
  const isoChoices = $derived.by(() => {
    const list = (opts?.isos || []).map((i) => ({ path: i.path, label: i.name }));
    for (const d of cfg?.disks || []) {
      if (d.device === 'cdrom' && d.source && !list.some((i) => i.path === d.source)) list.unshift({ path: d.source, label: d.source });
    }
    return list;
  });
  const netValue = (n) => `${n.kind}:${n.source}`;
  function setNet(n, v) {
    const i = v.indexOf(':');
    n.kind = v.slice(0, i);
    n.source = v.slice(i + 1);
  }
  const netChoices = $derived([
    ...(opts?.bridges || []).map((b) => ({ v: `bridge:${b}`, l: `${b} (bridge: its own address on that network)` })),
    ...(opts?.networks || []).map((n) => ({ v: `network:${n}`, l: `${n} (libvirt network)` })),
  ]);

  function addDisk() {
    cfg.disks.push({ target: '', device: 'disk', source: '', bus: vm?.os === 'windows' ? 'sata' : 'virtio', newSizeGB: 32, existing: false });
  }
  function addCD() {
    cfg.disks.push({ target: '', device: 'cdrom', source: '', bus: 'sata' });
  }
  function addNIC() {
    const first = netChoices.find((c) => c.v.includes('.')) || netChoices[0];
    const n = { index: 0, mac: '', kind: 'bridge', source: '', model: vm?.os === 'windows' ? 'e1000e' : 'virtio' };
    if (first) setNet(n, first.v);
    cfg.nics.push(n);
  }
  function removeRow(list, i) {
    if (list[i].target || list[i].index) list[i].remove = !list[i].remove;
    else list.splice(i, 1);
  }
  const usbKey = (u) => `${u.vendor}:${u.product}`.toLowerCase();
  const usbOn = (u) => cfg.usb.some((x) => usbKey(x) === usbKey(u));
  function toggleUSB(u) {
    if (usbOn(u)) cfg.usb = cfg.usb.filter((x) => usbKey(x) !== usbKey(u));
    else cfg.usb.push({ vendor: u.vendor, product: u.product });
  }
  const missingUSB = $derived((cfg?.usb || []).filter((u) => !(opts?.usb || []).some((h) => usbKey(h) === usbKey(u))));

  async function save() {
    const removing = cfg.disks.filter((d) => d.remove && d.device === 'disk');
    if (removing.length) {
      const ok = await confirm({
        title: `Detach ${removing.length === 1 ? 'a disk' : `${removing.length} disks`}?`,
        message: `The VM won't see ${removing.map((d) => d.source).join(', ')} any more. The image files themselves are kept.`,
        confirmLabel: 'Detach',
      });
      if (!ok) return;
    }
    saving = true;
    try {
      const body = {
        ...cfg,
        disks: cfg.disks.map(({ existing, ...d }) => ({ ...d, newSizeGB: +d.newSizeGB || 0, source: d.target || existing || d.device === 'cdrom' ? d.source : '' })),
        vcpus: +cfg.vcpus,
      };
      await api.put(`/vms/${encodeURIComponent(name)}/config`, body);
      toast(running ? 'Saved. Shut the VM down and start it again for the changes to apply (a reboot from inside isn\'t enough).' : 'Saved', 'success', running ? 9000 : 3000);
      await load();
      onsaved?.();
    } catch (e) {
      toast(e.message, 'error', 10000);
    }
    saving = false;
  }
</script>

{#if error}
  <div class="card err"><Icon name="alert" size={16} /> {error}</div>
{:else if !cfg || !opts}
  <div class="skeleton" style="height:420px"></div>
{:else}
  {#if running}
    <div class="note"><Icon name="info" size={16} /> The VM is running. Changes are saved now and apply the next time it starts from off.</div>
  {/if}
  <div class="stack-v">
    <section class="card">
      <div class="card-header"><h2>General</h2><span class="dim small">{cfg.firmware === 'uefi' ? 'UEFI' : 'BIOS'} · {cfg.machine}{cfg.cpuMode ? ` · CPU ${cfg.cpuMode}` : ''}</span></div>
      <div class="card-body stack-v">
        <label class="field"><span>Description</span><textarea rows="2" bind:value={cfg.description} placeholder="What this VM is for"></textarea></label>
      </div>
    </section>

    <section class="card">
      <div class="card-header"><h2>CPU and memory</h2><span class="dim small">This server: {opts.hostCPUs} CPU threads, {bytes(opts.hostMemory, 0)}</span></div>
      <div class="card-body grid2">
        <label class="field"><span>vCPUs</span>
          <div class="row"><input type="range" min="1" max={opts.hostCPUs} bind:value={cfg.vcpus} /><input class="num" type="number" min="1" max="256" bind:value={cfg.vcpus} /></div></label>
        <label class="field"><span>Memory (GB)</span>
          <div class="row"><input type="range" min="0.5" max={maxMemGB} step="0.5" bind:value={getMemGB, setMemGB} /><input class="num" type="number" min="0.25" step="0.25" bind:value={getMemGB, setMemGB} /></div></label>
      </div>
    </section>

    <section class="card">
      <div class="card-header"><h2>Disks and CD drives</h2><span class="spacer"></span>
        <button class="sm" onclick={addDisk}><Icon name="plus" size={13} /> Disk</button>
        <button class="sm" onclick={addCD}><Icon name="plus" size={13} /> CD drive</button></div>
      {#each cfg.disks as d, i}
        <div class="li" class:removed={d.remove}>
          <span class="badge kind">{d.device === 'cdrom' ? 'CD/DVD' : 'Disk'}</span>
          <div class="grow stack-s">
            {#if d.fixed}
              <span class="mono truncate" title={d.source}>{d.source}</span><span class="dim small">{d.target} · {d.bus} · edit in the XML tab</span>
            {:else if d.device === 'cdrom'}
              <select bind:value={d.source} disabled={d.remove} aria-label="ISO image">
                <option value="">Empty drive</option>
                {#each isoChoices as iso}<option value={iso.path}>{iso.label}</option>{/each}
              </select>
            {:else if d.target}
              <input class="mono" bind:value={d.source} disabled={d.remove} aria-label="Disk image" />
              <div class="row small">
                <span class="dim">{d.target}{d.format ? ` · ${d.format}` : ''}{d.size ? ` · ${bytes(d.size, 1)}` : ''}</span>
                {#if d.size}<label class="row inline">Grow to <input class="num sm" type="number" min={Math.ceil(d.size / 1024 ** 3)} placeholder={Math.ceil(d.size / 1024 ** 3)} bind:value={d.newSizeGB} disabled={d.remove} /> GB</label>{/if}
              </div>
            {:else}
              <div class="row small">
                <label class="row inline"><input type="radio" bind:group={d.existing} value={false} /> New</label>
                <label class="row inline"><input type="radio" bind:group={d.existing} value={true} /> Existing image</label>
              </div>
              {#if d.existing}
                <input class="mono" bind:value={d.source} placeholder="/mnt/user/domains/{name}/vdisk2.qcow2" aria-label="Image path" />
              {:else}
                <label class="row inline small"><input class="num sm" type="number" min="1" bind:value={d.newSizeGB} /> GB thin qcow2, saved next to the VM's other disks</label>
              {/if}
            {/if}
          </div>
          {#if !d.fixed}
            <select class="bus" bind:value={d.bus} disabled={d.remove} aria-label="Bus">
              {#each d.device === 'cdrom' ? ['sata', 'scsi', 'usb', 'ide'] : ['virtio', 'sata', 'scsi', 'usb', 'ide'] as b}<option value={b}>{busLabel[b]}</option>{/each}
              {#if ![...Object.keys(busLabel)].includes(d.bus)}<option value={d.bus}>{d.bus}</option>{/if}
            </select>
          {/if}
          <button class="sm ghost" onclick={() => removeRow(cfg.disks, i)}>{d.remove ? 'Undo' : d.target ? 'Detach' : 'Remove'}</button>
        </div>
      {:else}<div class="li dim">No disks</div>{/each}
      <div class="card-body tight">
        <div class="field inline"><span>Boot from</span>
          <div class="segmented">
            <button class:active={cfg.boot === 'disk'} onclick={() => (cfg.boot = 'disk')}>Hard disk</button>
            <button class:active={cfg.boot === 'cdrom'} onclick={() => (cfg.boot = 'cdrom')}>CD/DVD</button>
          </div>
        </div>
        <p class="hint">Windows guests need VirtIO drivers before a disk can move to the VirtIO bus. Disks can only be made bigger, and only while the VM is off; grow the partition inside the guest afterwards.</p>
      </div>
    </section>

    <section class="card">
      <div class="card-header"><h2>Network</h2><span class="spacer"></span><button class="sm" onclick={addNIC}><Icon name="plus" size={13} /> Network card</button></div>
      {#each cfg.nics as n, i}
        <div class="li" class:removed={n.remove}>
          <Icon name="network" size={15} />
          {#if n.fixed}
            <span class="grow">{n.source} <span class="dim small">({n.kind}: edit in the XML tab)</span></span>
          {:else}
            <select class="grow" value={netValue(n)} onchange={(e) => setNet(n, e.target.value)} disabled={n.remove} aria-label="Network">
              {#each netChoices as c}<option value={c.v}>{c.l}</option>{/each}
              {#if !netChoices.some((c) => c.v === netValue(n))}<option value={netValue(n)}>{n.source} ({n.kind}, not found on this host)</option>{/if}
            </select>
            <select class="model" bind:value={n.model} disabled={n.remove} aria-label="Model">
              {#each nicModels as m}<option value={m}>{m}</option>{/each}
              {#if !nicModels.includes(n.model)}<option value={n.model}>{n.model}</option>{/if}
            </select>
          {/if}
          <span class="mono dim small hide-sm">{n.mac || 'new'}</span>
          <button class="sm ghost" onclick={() => removeRow(cfg.nics, i)}>{n.remove ? 'Undo' : 'Remove'}</button>
        </div>
      {:else}<div class="li dim">No network</div>{/each}
    </section>

    <section class="card">
      <div class="card-header"><h2>Display</h2></div>
      <div class="card-body stack-v">
        <label class="toggle"><input type="checkbox" bind:checked={cfg.vnc} /> <span><strong>VNC console</strong><small>Lets you see the VM's screen on the Console tab. Only reachable through Homestack.</small></span></label>
        <label class="toggle"><input type="checkbox" bind:checked={cfg.serial} /> <span><strong>Serial port</strong><small>A text console on the Serial tab that keeps working when the screen freezes. Takes effect the next time the VM starts.</small></span></label>
        <label class="field inline"><span>Display adapter</span>
          <select bind:value={cfg.video}>
            {#each videos as [v, l]}<option value={v}>{l}</option>{/each}
            {#if !videos.some(([v]) => v === cfg.video)}<option value={cfg.video}>{cfg.video}</option>{/if}
          </select></label>
      </div>
    </section>

    <section class="card">
      <div class="card-header"><h2>USB devices</h2><span class="dim small">Passed straight to the VM, like a Zigbee or Z-Wave stick</span></div>
      {#each opts.usb as u}
        <label class="li click"><input type="checkbox" checked={usbOn(u)} onchange={() => toggleUSB(u)} /> <span class="grow">{u.name || 'USB device'}</span><span class="mono dim small">{u.vendor.slice(2)}:{u.product.slice(2)}</span></label>
      {/each}
      {#each missingUSB as u}
        <label class="li click"><input type="checkbox" checked onchange={() => toggleUSB(u)} /> <span class="grow">Not plugged in now</span><span class="mono dim small">{u.vendor.slice(2)}:{u.product.slice(2)}</span></label>
      {/each}
      {#if !opts.usb.length && !missingUSB.length}<div class="li dim">No USB devices found on this server.</div>{/if}
    </section>

    {#if cfg.kept.length}
      <p class="dim small"><Icon name="info" size={13} /> Kept as they are (change them in the XML tab): {cfg.kept.join('; ')}.</p>
    {/if}

    <div class="row actions">
      <span class="spacer"></span>
      {#if dirty}<button onclick={load}>Discard</button>{/if}
      <button class="primary" disabled={!dirty || saving} onclick={save}>
        {#if saving}<Icon name="loader" size={15} class="spin" />{:else}<Icon name="save" size={15} />{/if} Save settings
      </button>
    </div>
  </div>
{/if}

<style>
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 14px; font-size: 13.5px; background: var(--accent-soft); color: var(--accent-text); }
  .err { display: flex; gap: 8px; align-items: center; padding: 12px 16px; color: var(--red-text); }
  .grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .field.inline { display: flex; align-items: center; gap: 12px; }
  .field.inline select { width: auto; }
  .row { display: flex; gap: 8px; align-items: center; }
  .row.inline { gap: 5px; font-weight: 400; }
  input[type='range'] { flex: 1; height: auto; padding: 0; border: 0; accent-color: var(--accent); }
  .num { width: 84px; flex: none; }
  .num.sm { width: 70px; height: 28px; }
  .li { display: flex; align-items: center; gap: 10px; padding: 10px 18px; border-bottom: 1px solid var(--border); font-size: 13.5px; min-width: 0; }
  .li.click { cursor: pointer; }
  .li.removed .grow, .li.removed .kind { opacity: 0.45; text-decoration: line-through; }
  .kind { flex: none; min-width: 58px; justify-content: center; }
  .grow { flex: 1; min-width: 0; }
  .stack-s { display: grid; gap: 5px; }
  .bus { width: auto; flex: none; max-width: 170px; }
  .model { width: auto; flex: none; }
  .small { font-size: 12.5px; }
  .tight { padding-top: 12px; padding-bottom: 14px; display: grid; gap: 8px; }
  .toggle { display: flex; gap: 10px; align-items: flex-start; cursor: pointer; }
  .toggle input { margin-top: 3px; width: 16px; height: 16px; accent-color: var(--accent); flex: none; }
  .toggle span { display: grid; }
  .toggle small { color: var(--text-3); font-size: 12.5px; }
  .actions { padding-bottom: 8px; }
  @media (max-width: 700px) { .grid2 { grid-template-columns: 1fr; } .li { flex-wrap: wrap; } }
</style>
