<script>
  // Adds a VM template: an official cloud image, any image link with a
  // checksum, or a copy of a prepared VM's disk. The work runs as a job (see
  // JobsPanel), so the dialog closes once it has started.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { runJob, toast } from '../lib/state.svelte.js';
  import { copy } from '../lib/clipboard.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from './Modal.svelte';

  let { start = { tab: 'catalog' }, catalog = [], onclose, onchange } = $props();

  // svelte-ignore state_referenced_locally
  let tab = $state(start.tab);
  let error = $state('');
  let busy = $state(false);
  let vms = $state(null);

  // svelte-ignore state_referenced_locally
  let form = $state({
    image: catalog.find((c) => c.recommended)?.id || catalog[0]?.id || '',
    url: '',
    checksum: '',
    name: '',
    description: '',
    vm: start.vm || '',
    cloudInit: true,
    prepared: false,
  });

  // The catalog may arrive after the dialog opens: preselect once it does.
  $effect(() => {
    if (!form.image && catalog.length) form.image = (catalog.find((c) => c.recommended) || catalog[0]).id;
  });

  onMount(async () => {
    try {
      vms = await api.get('/vms');
      const v = vms.find((x) => x.name === form.vm);
      if (v) form.cloudInit = v.os !== 'windows';
    } catch {
      vms = [];
    }
  });

  const chosenVM = $derived(vms?.find((v) => v.name === form.vm));
  const windows = $derived(/win/i.test(chosenVM?.os || ''));
  const prep = $derived(
    form.cloudInit
      ? 'sudo cloud-init clean --logs --seed\nsudo truncate -s 0 /etc/machine-id\nsudo poweroff'
      : 'sudo truncate -s 0 /etc/machine-id\nsudo poweroff',
  );

  const tabs = [
    ['catalog', 'Cloud image', 'download'],
    ['url', 'From a link', 'globe'],
    ['vm', 'From a VM', 'vm'],
  ];

  const copyPrep = () => copy(prep);

  async function submit(e) {
    e.preventDefault();
    error = '';
    let body, label;
    if (tab === 'catalog') {
      const img = catalog.find((c) => c.id === form.image);
      body = { kind: 'catalog', image: form.image, name: form.name.trim(), description: form.description.trim() };
      label = form.name.trim() || img?.name;
    } else if (tab === 'url') {
      if (!/^https:\/\//i.test(form.url.trim())) return (error = 'Use an https:// link.');
      if (!form.checksum.trim()) return (error = "Enter the image's checksum, or a link to its checksum list. Homestack won't use an image it can't verify.");
      body = { kind: 'url', url: form.url.trim(), checksum: form.checksum.trim(), name: form.name.trim(), description: form.description.trim(), cloudInit: form.cloudInit };
      label = form.name.trim() || form.url.trim().split('/').pop();
    } else {
      if (!chosenVM) return (error = 'Choose a VM.');
      if (chosenVM.state !== 'shutoff') return (error = `Shut ${chosenVM.name} down first.`);
      body = { kind: 'vm', vm: form.vm, name: form.name.trim(), description: form.description.trim(), cloudInit: form.cloudInit && !windows, prepared: form.prepared };
      label = form.name.trim() || form.vm;
    }
    busy = true;
    const job = runJob(`Template: ${label}`, 'POST', '/vm-templates', body).then((res) => {
      onchange?.();
      return res;
    });
    setTimeout(() => onchange?.(), 700); // show it as in progress
    // Mistakes in the form come back at once: keep the dialog open for those.
    const quick = await Promise.race([job, new Promise((r) => setTimeout(() => r(null), 1500))]);
    busy = false;
    if (quick && !quick.ok) {
      error = quick.error;
      return;
    }
    onclose?.();
  }
</script>

<Modal title="Add a VM template" {onclose} width={640}>
  <form id="addtpl" class="stack-v" onsubmit={submit}>
    <div class="segmented">
      {#each tabs as [id, label, icon]}
        <button type="button" class:active={tab === id} onclick={() => { tab = id; error = ''; }}><Icon name={icon} size={14} /> {label}</button>
      {/each}
    </div>

    {#if tab === 'catalog'}
      <p class="hint">Official images, straight from each publisher over HTTPS, checked against the checksums they publish. Always their latest build.</p>
      <div class="kinds">
        {#each catalog as img (img.id)}
          <button type="button" class="kind" class:active={form.image === img.id} onclick={() => (form.image = img.id)}>
            <Icon name="disc" size={16} />
            <strong>{img.name}{#if img.recommended} <span class="badge accent tiny">Recommended</span>{/if}</strong>
            <small>{img.note} About {img.approxMB} MB to download.</small>
          </button>
        {/each}
      </div>
    {:else if tab === 'url'}
      <label class="field"><span>Image link</span>
        <input class="mono" type="url" bind:value={form.url} placeholder="https://…/image.qcow2" required autocapitalize="off" spellcheck="false" />
        <span class="hint">A qcow2 or raw disk image (.qcow2, .img, .raw). Compressed files (.xz, .tar.gz) aren't supported.</span></label>
      <label class="field"><span>Checksum</span>
        <input class="mono" bind:value={form.checksum} placeholder="SHA-256 / SHA-512 value, or https://…/SHA256SUMS" required autocapitalize="off" spellcheck="false" />
        <span class="hint">Required: the download is deleted unless it matches. Copy it from the page you got the link from.</span></label>
      <label class="chk"><input type="checkbox" bind:checked={form.cloudInit} /> <span>It's a cloud image with cloud-init (official cloud images are)</span></label>
    {:else}
      <label class="field"><span>VM</span>
        <select bind:value={form.vm} required onchange={() => (form.cloudInit = !/win/i.test(chosenVM?.os || ''))}>
          <option value="" disabled>Choose a VM</option>
          {#each vms || [] as v (v.uuid)}
            <option value={v.name} disabled={v.state !== 'shutoff'}>{v.name}{v.state !== 'shutoff' ? ' (shut it down first)' : ''}</option>
          {/each}
        </select>
        <span class="hint">Its first disk is copied into the template. The VM itself isn't changed, and keeps working.</span></label>
      {#if windows}
        <div class="note"><Icon name="info" size={16} /><span>For Windows, run <code>sysprep /generalize /oobe /shutdown</code> in the VM first, so each copy gets its own identity. Copies are set up by Windows' own first-run screens, not cloud-init.</span></div>
      {:else if form.vm}
        <label class="chk"><input type="checkbox" bind:checked={form.cloudInit} /> <span>It has cloud-init installed (so each copy can be given its own hostname, login and network)</span></label>
        <div class="prep">
          <div class="prep-head"><strong>Prepare it first</strong><button type="button" class="ghost sm" onclick={copyPrep}><Icon name="copy" size={13} /> Copy</button></div>
          <p>Run these inside the VM, so copies don't share its identity:</p>
          <pre>{prep}</pre>
          {#if form.cloudInit}
            <p class="dim">This clears cloud-init's record of having run and the machine ID. Each copy then makes its own machine ID and SSH host keys on first boot. The VM itself gets a new machine ID when it next starts, so its DHCP address may change.</p>
          {:else}
            <p class="dim">Without cloud-init, copies start with this VM's hostname and SSH host keys. Change them in each copy, or install cloud-init (<code>sudo apt install cloud-init</code>) before making the template.</p>
          {/if}
        </div>
      {/if}
      {#if form.vm}
        <label class="chk"><input type="checkbox" bind:checked={form.prepared} /> <span>{windows ? "I've run sysprep and it shut down" : "I've run these and the VM is shut down"}</span></label>
      {/if}
    {/if}

    <div class="grid2">
      <label class="field"><span>Name <small class="dim">(optional)</small></span>
        <input bind:value={form.name} maxlength="80" placeholder={tab === 'catalog' ? catalog.find((c) => c.id === form.image)?.name : tab === 'vm' ? form.vm || 'My template' : 'My template'} /></label>
      <label class="field"><span>Notes <small class="dim">(optional)</small></span>
        <input bind:value={form.description} maxlength="500" placeholder="What's in it" /></label>
    </div>
    {#if error}<p class="error-text">{error}</p>{/if}
  </form>
  {#snippet footer()}
    <button onclick={() => onclose?.()}>Cancel</button>
    <button class="primary" type="submit" form="addtpl" disabled={busy || (tab === 'vm' && (!form.vm || !form.prepared))}>
      {#if busy}<Icon name="loader" size={15} class="spin" />{:else}<Icon name={tab === 'vm' ? 'copy' : 'download'} size={15} />{/if}
      {tab === 'vm' ? 'Copy into a template' : 'Download'}
    </button>
  {/snippet}
</Modal>

<style>
  .segmented button { display: inline-flex; align-items: center; gap: 6px; }
  .hint { margin: 0; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
  .chk { display: flex; gap: 8px; align-items: flex-start; font-size: 13.5px; cursor: pointer; }
  .chk input { margin-top: 3px; }
  .kinds { display: grid; gap: 8px; grid-template-columns: 1fr 1fr; }
  .kind { all: unset; box-sizing: border-box; display: grid; grid-template-columns: 22px 1fr; column-gap: 8px; row-gap: 2px; padding: 10px 12px; border: 1px solid var(--border); border-radius: 10px; cursor: pointer; }
  .kind :global(svg) { grid-row: span 2; margin-top: 2px; color: var(--text-3); }
  .kind strong { font-size: 13.5px; display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
  .kind small { color: var(--text-2); font-size: 12.5px; line-height: 1.45; }
  .kind:hover { border-color: var(--border-strong, var(--text-3)); }
  .kind.active { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); }
  .kind.active :global(svg) { color: var(--accent); }
  .kind:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  .tiny { height: 18px; font-size: 11px; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; font-size: 13px; background: var(--surface-2); line-height: 1.5; }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .prep { border: 1px solid var(--border); border-radius: 10px; padding: 12px 14px; display: grid; gap: 8px; font-size: 13px; }
  .prep-head { display: flex; align-items: center; justify-content: space-between; }
  .prep p { margin: 0; line-height: 1.5; }
  .prep pre { margin: 0; padding: 10px 12px; background: var(--surface-2); border-radius: 8px; font-size: 12.5px; overflow-x: auto; }
  @media (max-width: 640px) { .kinds, .grid2 { grid-template-columns: 1fr; } }
</style>
