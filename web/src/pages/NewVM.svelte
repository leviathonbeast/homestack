<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import { bytes } from '../lib/format.js';
  import { navigate, query } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import CloudInitFields from '../components/CloudInitFields.svelte';

  let opts = $state(null);
  let error = $state('');
  let busy = $state(false);

  // Where the VM comes from: a template (cloned disk, set up by cloud-init)
  // or an installer ISO.
  let source = $state('iso');
  let templates = $state([]);
  let templateId = $state('');
  let clone = $state('linked');
  let ci = $state({
    hostname: '',
    user: '',
    password: '',
    sshKeys: '',
    sshPassword: false,
    network: { mode: 'dhcp', address: '', gateway: '', dns: '' },
    upgrade: true,
    guestAgent: true,
  });
  const tpl = $derived(templates.find((t) => t.id === templateId));
  const minDiskGB = $derived(tpl ? Math.ceil(tpl.size / 1024 ** 3) : 1);
  const loginOK = $derived(!tpl?.cloudInit || ci.password || ci.sshKeys.split('\n').some((l) => l.trim() && !l.trim().startsWith('#')));

  function pickTemplate(id) {
    templateId = id;
    const t = templates.find((x) => x.id === id);
    if (t) form.diskGB = Math.max(+form.diskGB || 0, Math.ceil(t.size / 1024 ** 3), 20);
  }

  let form = $state({
    name: '',
    os: 'linux',
    vcpus: 2,
    memoryGB: 4,
    firmware: 'uefi',
    diskMode: 'new',
    diskGB: 32,
    diskPath: '',
    iso: '',
    virtioIso: '',
    network: '',
    tpm: true,
    start: true,
    usb: [],
  });
  const usbKey = (u) => `${u.vendor}:${u.product}`;
  function toggleUSB(u) {
    form.usb = form.usb.includes(usbKey(u)) ? form.usb.filter((k) => k !== usbKey(u)) : [...form.usb, usbKey(u)];
  }

  onMount(async () => {
    try {
      const tplData = await api.get('/vm-templates').catch(() => null);
      if (tplData) {
        templates = tplData.templates.filter((t) => !t.missing);
        ci.user = tplData.defaults?.user || '';
        ci.sshKeys = (tplData.defaults?.sshKeys || []).join('\n');
        const wanted = query('template');
        if (templates.length) {
          source = 'template';
          pickTemplate(templates.some((t) => t.id === wanted) ? wanted : templates[0].id);
        }
      }
      opts = await api.get('/vms/options');
      // Prefer the VM VLAN bridge if there is one (like br0.40), else the main bridge.
      const vlan = opts.bridges.find((b) => b.includes('.'));
      form.network = vlan ? `bridge:${vlan}` : opts.bridges[0] ? `bridge:${opts.bridges[0]}` : opts.networks[0] ? `network:${opts.networks[0]}` : '';
      form.tpm = !!opts.caps.tpm;
      if (!opts.caps.loaders.length) form.firmware = 'bios';
    } catch (e) {
      error = e.message;
    }
  });

  function setOS(os) {
    form.os = os;
    if (os === 'windows') {
      form.vcpus = Math.max(form.vcpus, 4);
      form.memoryGB = Math.max(form.memoryGB, 8);
      form.diskGB = Math.max(form.diskGB, 80);
    }
  }

  const maxMem = $derived(opts ? Math.max(1, Math.floor(opts.hostMemory / 1024 ** 3)) : 64);
  const diskPreview = $derived(form.name && opts?.domainsDir ? `${opts.domainsDir}/${form.name}/vdisk1.qcow2` : '');
  const kvm = $derived(opts?.caps.domainType === 'kvm');

  async function create(e) {
    e.preventDefault();
    error = '';
    busy = true;
    const [kind, netSource] = form.network ? form.network.split(':') : ['', ''];
    const common = {
      name: form.name.trim(),
      vcpus: +form.vcpus,
      memoryMB: Math.round(+form.memoryGB * 1024),
      network: { kind, source: netSource },
      start: form.start,
      usb: form.usb.map((k) => ({ vendor: k.split(':')[0], product: k.split(':')[1] })),
    };
    const body =
      source === 'template'
        ? {
            ...common,
            template: templateId,
            clone,
            disk: { sizeGB: +form.diskGB },
            cloudInit: tpl?.cloudInit
              ? {
                  hostname: ci.hostname.trim(),
                  user: ci.user.trim(),
                  password: ci.password,
                  sshKeys: [ci.sshKeys],
                  sshPassword: ci.sshPassword && !!ci.password,
                  network: { ...ci.network, dns: [ci.network.dns] },
                  upgrade: ci.upgrade,
                  guestAgent: ci.guestAgent,
                }
              : undefined,
          }
        : {
            ...common,
            os: form.os,
            firmware: form.firmware,
            disk: { mode: form.diskMode, sizeGB: +form.diskGB, path: form.diskPath.trim() },
            iso: form.iso,
            virtioIso: form.os === 'windows' ? form.virtioIso : '',
            tpm: form.os === 'windows' && form.tpm,
          };
    // A cloud image shows its first boot (and cloud-init's progress) on the serial console.
    const tab = !form.start ? '' : source === 'template' && tpl?.cloudInit ? '/serial' : '/console';
    try {
      await api.post('/vms', body);
      toast(`Created ${body.name}`, 'success');
      navigate(`/vms/${encodeURIComponent(body.name)}${tab}`);
    } catch (err) {
      error = err.message;
    }
    busy = false;
  }
</script>

<div class="page narrow">
  <a class="back" href="/vms"><Icon name="arrow-left" size={14} /> Virtual machines</a>
  <div class="page-header"><div><h1>New VM</h1><p class="sub">{source === 'template' ? 'Clone a template: ready in seconds, set up on first boot.' : 'Create a virtual machine and boot it from an installer ISO.'}</p></div></div>

  {#if !opts && !error}
    <div class="skeleton" style="height:400px"></div>
  {:else if opts}
    {#if !kvm}
      <div class="note amber"><Icon name="alert" size={16} /> KVM isn't available on this host, so VMs will run in slow software emulation. Enable virtualisation (VT-x / AMD-V) in the BIOS.</div>
    {/if}
    <form class="stack-v" onsubmit={create}>
      <div class="sources">
        <button type="button" class="kind" class:active={source === 'template'} onclick={() => (source = 'template')}>
          <Icon name="copy" size={18} /><strong>From a template</strong>
          <small>{templates.length ? 'Clone a ready-made disk. Ready in seconds.' : 'No templates yet: add one first.'}</small>
        </button>
        <button type="button" class="kind" class:active={source === 'iso'} onclick={() => (source = 'iso')}>
          <Icon name="disc" size={18} /><strong>From an installer ISO</strong>
          <small>Install the OS yourself, like on a real PC.</small>
        </button>
      </div>

      {#if source === 'template' && !templates.length}
        <div class="card empty">
          <h3>No templates yet</h3>
          <p>Download an official cloud image (Debian, Ubuntu…) as a template, then come back here.</p>
          <a class="btn primary" href="/vms/templates"><Icon name="plus" size={16} /> Add a template</a>
        </div>
      {:else}
      <section class="card">
        <div class="card-header"><h2>Basics</h2></div>
        <div class="card-body stack-v">
          <label class="field"><span>Name</span>
            <input bind:value={form.name} placeholder={source === 'template' ? 'web01' : 'home-assistant'} required pattern="[A-Za-z0-9][A-Za-z0-9._\-]*" autocapitalize="off" spellcheck="false" /></label>
          {#if source === 'template'}
            <label class="field"><span>Template</span>
              <select value={templateId} onchange={(e) => pickTemplate(e.currentTarget.value)}>
                {#each templates as t (t.id)}<option value={t.id}>{t.name} ({bytes(t.size, 0)}{t.cloudInit ? ', cloud-init' : ''})</option>{/each}
              </select>
              {#if tpl?.description}<span class="hint">{tpl.description}</span>{/if}
            </label>
          {:else}
          <div class="field"><span>Operating system</span>
            <div class="segmented">
              {#each [['linux', 'Linux'], ['windows', 'Windows'], ['other', 'Other']] as [k, l]}
                <button type="button" class:active={form.os === k} onclick={() => setOS(k)}>{l}</button>
              {/each}
            </div>
            <span class="hint">{form.os === 'windows' ? 'Uses devices Windows supports out of the box, plus Hyper-V tweaks for speed.' : 'Uses fast virtio devices, which Linux supports out of the box.'}</span>
          </div>
          {/if}
        </div>
      </section>

      <section class="card">
        <div class="card-header"><h2>Resources</h2><span class="dim small">This server: {opts.hostCPUs} CPU threads, {bytes(opts.hostMemory, 0)}</span></div>
        <div class="card-body grid2">
          <label class="field"><span>vCPUs</span>
            <div class="row"><input type="range" min="1" max={opts.hostCPUs} bind:value={form.vcpus} /><input class="num" type="number" min="1" max="128" bind:value={form.vcpus} /></div></label>
          <label class="field"><span>Memory (GB)</span>
            <div class="row"><input type="range" min="1" max={maxMem} step="1" bind:value={form.memoryGB} /><input class="num" type="number" min="0.5" step="0.5" bind:value={form.memoryGB} /></div></label>
        </div>
      </section>

      {#if source === 'template'}
      <section class="card">
        <div class="card-header"><h2>Disk</h2></div>
        <div class="card-body stack-v">
          <div class="kinds">
            <button type="button" class="kind" class:active={clone === 'linked'} onclick={() => (clone = 'linked')}>
              <Icon name="layers" size={16} /><strong>Linked clone</strong>
              <small>Ready instantly and stores only what this VM changes. Reads the rest from the template, so the template can't be deleted while this VM exists.</small>
            </button>
            <button type="button" class="kind" class:active={clone === 'full'} onclick={() => (clone = 'full')}>
              <Icon name="copy" size={16} /><strong>Full clone</strong>
              <small>A complete, independent copy of the template's disk. Takes a minute or two; doesn't depend on the template afterwards.</small>
            </button>
          </div>
          <label class="field"><span>Size (GB)</span><input type="number" min={minDiskGB} max="65536" bind:value={form.diskGB} style="max-width:160px" /></label>
          <span class="hint">At least {minDiskGB} GB (the template's size). The VM grows its partition to fill it on first boot. Saved as <code>{opts.domainsDir}/{form.name || '<name>'}/vdisk1.qcow2</code>.</span>
        </div>
      </section>
      {:else}
      <section class="card">
        <div class="card-header"><h2>Storage</h2></div>
        <div class="card-body stack-v">
          <div class="segmented">
            {#each [['new', 'New disk'], ['existing', 'Existing disk'], ['none', 'No disk']] as [k, l]}
              <button type="button" class:active={form.diskMode === k} onclick={() => (form.diskMode = k)}>{l}</button>
            {/each}
          </div>
          {#if form.diskMode === 'new'}
            <label class="field"><span>Size (GB)</span><input type="number" min="1" max="65536" bind:value={form.diskGB} style="max-width:160px" /></label>
            <span class="hint">A thin qcow2 file: it only uses space as the VM writes data.{#if diskPreview}{' '}Saved as <code>{diskPreview}</code>.{/if}</span>
          {:else if form.diskMode === 'existing'}
            <label class="field"><span>Disk image path</span><input class="mono" bind:value={form.diskPath} placeholder="/mnt/user/domains/old-vm/vdisk1.qcow2" required /></label>
          {/if}
        </div>
      </section>

      <section class="card">
        <div class="card-header"><h2>Install from</h2></div>
        <div class="card-body stack-v">
          <label class="field"><span>Installer ISO</span>
            <select bind:value={form.iso}>
              <option value="">None</option>
              {#each opts.isos as iso}<option value={iso.path}>{iso.name} ({bytes(iso.size, 1)})</option>{/each}
            </select>
            <span class="hint">{opts.isos.length ? `From ${opts.isosDir}.` : `No ISOs found in ${opts.isosDir}. Put some there, or change the folder in Settings → Stacks and VMs.`}</span>
          </label>
          {#if form.os === 'windows'}
            <label class="field"><span>VirtIO drivers ISO (optional)</span>
              <select bind:value={form.virtioIso}>
                <option value="">None</option>
                {#each opts.isos as iso}<option value={iso.path}>{iso.name}</option>{/each}
              </select>
              <span class="hint">Lets Windows use faster virtio disks and networking after installing.</span>
            </label>
          {/if}
        </div>
      </section>
      {/if}

      <section class="card">
        <div class="card-header"><h2>Network and firmware</h2></div>
        <div class="card-body stack-v">
          <label class="field"><span>Network</span>
            <select bind:value={form.network}>
              {#each opts.bridges as b}<option value="bridge:{b}">{b} (bridge: the VM gets its own address on that network)</option>{/each}
              {#each opts.networks as n}<option value="network:{n}">{n} (libvirt network)</option>{/each}
              <option value="">No network</option>
            </select>
          </label>
          {#if source === 'template'}
            <p class="hint">Firmware: {tpl?.firmware === 'bios' ? 'BIOS' : 'UEFI'}, the same as the template.{#if tpl?.firmware !== 'bios' && !opts.caps.loaders.length} <strong>This host has no UEFI firmware (OVMF): install it first.</strong>{/if}</p>
          {:else}
          <div class="field"><span>Firmware</span>
            <div class="segmented">
              <button type="button" class:active={form.firmware === 'uefi'} disabled={!opts.caps.loaders.length} onclick={() => (form.firmware = 'uefi')}>UEFI</button>
              <button type="button" class:active={form.firmware === 'bios'} onclick={() => (form.firmware = 'bios')}>BIOS</button>
            </div>
            <span class="hint">UEFI suits almost every modern OS.</span>
          </div>
          {/if}
          {#if source === 'iso' && form.os === 'windows'}
            <label class="chk"><input type="checkbox" bind:checked={form.tpm} disabled={!opts.caps.tpm} /> <span>Add a TPM 2.0 (Windows 11 needs one){#if !opts.caps.tpm} (install swtpm on the host to enable){/if}</span></label>
          {/if}
        </div>
      </section>

      {#if opts.usb?.length}
        <section class="card">
          <div class="card-header"><h2>USB devices</h2><span class="dim small">Optional: passed straight to the VM</span></div>
          {#each opts.usb as u}
            <label class="usb"><input type="checkbox" checked={form.usb.includes(usbKey(u))} onchange={() => toggleUSB(u)} /> <span class="grow">{u.name || 'USB device'}</span><span class="mono dim small">{u.vendor.slice(2)}:{u.product.slice(2)}</span></label>
          {/each}
        </section>
      {/if}

      {#if source === 'template' && tpl?.cloudInit}
        <CloudInitFields bind:ci vmName={form.name} hasNIC={!!form.network} />
      {:else if source === 'template' && tpl}
        <p class="hint">This template doesn't use cloud-init, so the new VM starts as an exact copy, with the template's hostname and logins.</p>
      {/if}

      <p class="hint">After creating it, the VM's Settings tab changes CPUs, memory, disks, CDs, network and USB devices; the XML tab is there for anything else.</p>
      {#if error}<p class="error-text">{error}</p>{/if}
      <div class="row">
        <label class="chk"><input type="checkbox" bind:checked={form.start} /> <span>Start it and open the {source === 'template' && tpl?.cloudInit ? 'serial console (shows the first-boot setup)' : 'console'}</span></label>
        <span class="spacer"></span>
        <button class="primary" type="submit" disabled={busy || !form.name.trim() || (source === 'template' && (!tpl || !loginOK || +form.diskGB < minDiskGB))}>
          {#if busy}<Icon name="loader" size={15} class="spin" />{:else}<Icon name="plus" size={15} />{/if} Create VM
        </button>
      </div>
      {/if}
    </form>
  {:else}
    <div class="card err"><Icon name="alert" size={16} /> {error}</div>
  {/if}
</div>

<style>
  .narrow > :global(*) { max-width: 860px; }
  .back { display: inline-flex; align-items: center; gap: 5px; color: var(--text-2); font-size: 13px; margin-bottom: 12px; }
  .grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  input[type='range'] { flex: 1; height: auto; padding: 0; border: 0; accent-color: var(--accent); }
  .row { display: flex; gap: 8px; align-items: center; }
  .num { width: 84px; flex: none; }
  .usb { display: flex; gap: 10px; align-items: center; padding: 10px 18px; border-bottom: 1px solid var(--border); font-size: 13.5px; cursor: pointer; }
  .usb:last-child { border-bottom: 0; }
  .grow { flex: 1; min-width: 0; }
  .small { font-size: 12.5px; }
  .chk { display: flex; gap: 8px; align-items: center; font-size: 13.5px; cursor: pointer; }
  .spacer { flex: 1; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 14px; font-size: 13.5px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .err { display: flex; gap: 8px; align-items: center; padding: 12px 16px; color: var(--red-text); }
  .sources, .kinds { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
  .kind { all: unset; box-sizing: border-box; display: grid; grid-template-columns: 24px 1fr; column-gap: 8px; row-gap: 2px; padding: 12px 14px; border: 1px solid var(--border); border-radius: 12px; cursor: pointer; background: var(--surface); }
  .kind :global(svg) { grid-row: span 2; margin-top: 2px; color: var(--text-3); }
  .kind strong { font-size: 14px; }
  .kind small { color: var(--text-2); font-size: 12.5px; line-height: 1.45; }
  .kind:hover { border-color: var(--text-3); }
  .kind.active { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); }
  .kind.active :global(svg) { color: var(--accent); }
  .kind:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  @media (max-width: 700px) { .grid2, .sources, .kinds { grid-template-columns: 1fr; } }
</style>
