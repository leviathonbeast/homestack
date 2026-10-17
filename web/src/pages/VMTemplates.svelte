<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { bytes, ago } from '../lib/format.js';
  import { confirm, toast } from '../lib/state.svelte.js';
  import { navigate, query } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';
  import Modal from '../components/Modal.svelte';
  import AddTemplate from '../components/AddTemplate.svelte';

  let data = $state(null);
  let error = $state('');
  let adding = $state(null); // { tab, vm }
  let editing = $state(null);
  let saving = $state(false);

  async function load() {
    try {
      data = await api.get('/vm-templates');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }

  onMount(() => {
    load();
    const from = query('from');
    if (from) {
      adding = { tab: 'vm', vm: from };
      navigate('/vms/templates', { replace: true });
    }
    // Poll only while something is downloading or copying.
    const t = setInterval(() => data?.busy.length && load(), 2000);
    return () => clearInterval(t);
  });

  async function remove(t) {
    const linked = t.vms.length ? ` ${t.vms.length} VM${t.vms.length > 1 ? 's were' : ' was'} made from it (${t.vms.join(', ')}); linked clones among them would stop working, so Homestack keeps the template until they're gone.` : '';
    const ok = await confirm({
      title: `Delete template ${t.name}?`,
      message: `This deletes its disk image, ${t.path}.${linked} VMs made as full clones are unaffected.`,
      confirmLabel: 'Delete template',
      danger: true,
      typeToConfirm: t.name,
    });
    if (!ok) return;
    try {
      await api.del(`/vm-templates/${encodeURIComponent(t.id)}`);
      toast(`Deleted template ${t.name}`, 'success');
      load();
    } catch (e) {
      toast(e.message, 'error', 10000);
    }
  }

  async function save(e) {
    e.preventDefault();
    saving = true;
    try {
      await api.put(`/vm-templates/${encodeURIComponent(editing.id)}`, { name: editing.name, description: editing.description, cloudInit: editing.cloudInit });
      editing = null;
      load();
    } catch (err) {
      toast(err.message, 'error');
    }
    saving = false;
  }

  const phase = { starting: 'Starting', checksum: 'Fetching checksums', downloading: 'Downloading', copying: 'Copying the disk', saving: 'Checking' };
</script>

<div class="page">
  <a class="back" href="/vms"><Icon name="arrow-left" size={14} /> Virtual machines</a>
  <div class="page-header">
    <div>
      <h1>VM templates</h1>
      <p class="sub">Ready-made disks to create VMs from in seconds, instead of installing from an ISO each time.</p>
    </div>
    <button class="primary" onclick={() => (adding = { tab: 'catalog' })}><Icon name="plus" size={16} /> Add template</button>
  </div>

  <div class="intro card">
    <Icon name="copy" size={20} />
    <div class="grow">
      <p><strong>How templates work.</strong> A template is a disk image that's never changed. New VMs are cloned from it:</p>
      <ul>
        <li><strong>Linked clone</strong>: ready instantly and stores only what the VM changes. It reads the rest from the template, so the template must be kept.</li>
        <li><strong>Full clone</strong>: an independent copy. Takes a minute or two, works on its own.</li>
      </ul>
      <p>Cloud images set themselves up on first boot with <em>cloud-init</em>: you choose the hostname, login, SSH keys and network when you create the VM.</p>
    </div>
  </div>

  {#if error}<div class="card err"><Icon name="alert" size={16} /> {error}</div>{/if}

  {#if data?.busy.length}
    <div class="card busy">
      {#each data.busy as b (b.id)}
        <div class="trow">
          <div class="ico on"><Icon name="loader" size={18} class="spin" /></div>
          <div class="main">
            <strong>{b.name}</strong>
            <div class="meta"><span>{phase[b.phase] || b.phase}</span>{#if b.total}<span>{bytes(b.done)} of {bytes(b.total)}</span>{/if}</div>
            {#if b.total}<div class="meter"><div style="width:{Math.min(100, (b.done / b.total) * 100)}%"></div></div>{/if}
          </div>
        </div>
      {/each}
    </div>
  {/if}

  <div class="card">
    {#if data === null && !error}
      {#each Array(2) as _}<div class="trow"><div class="skeleton" style="height:18px;width:40%"></div></div>{/each}
    {:else if data && !data.templates.length}
      <div class="empty">
        <h3>No templates yet</h3>
        <p>Download an official cloud image (Debian, Ubuntu, AlmaLinux, Rocky) or turn a prepared VM into one.</p>
        <button class="primary" onclick={() => (adding = { tab: 'catalog' })}><Icon name="plus" size={16} /> Add template</button>
      </div>
    {:else}
      {#each data?.templates || [] as t (t.id)}
        <div class="trow">
          <div class="ico"><Icon name="disc" size={20} /></div>
          <div class="main">
            <div class="row">
              <strong>{t.name}</strong>
              {#if t.missing}<span class="badge red" title={t.path}>Image missing</span>{/if}
              {#if t.cloudInit}<span class="badge" title="Set up on first boot: hostname, login, SSH keys, network">cloud-init</span>{/if}
              <span class="badge">{t.firmware === 'bios' ? 'BIOS' : 'UEFI'}</span>
            </div>
            {#if t.description}<div class="desc">{t.description}</div>{/if}
            <div class="meta">
              <span class="src" title={t.checksum ? `Checksum verified: ${t.checksum}` : ''}>{t.source}{#if t.checksum}<Icon name="shield" size={12} />{/if}</span>
              {#if !t.missing}<span>{bytes(t.size, 0)} disk · {bytes(t.used)} on the server</span>{/if}
              <span class="hide-sm">Added {ago(t.created)}</span>
              {#if t.vms.length}<span title={t.vms.join(', ')}>Used by {t.vms.length} VM{t.vms.length > 1 ? 's' : ''}</span>{/if}
            </div>
            {#if t.missing}<div class="desc red-text">The image <code>{t.path}</code> isn't there any more. Delete this template, or put the file back.</div>{/if}
          </div>
          <div class="acts">
            <button class="sm" disabled={t.missing} onclick={() => navigate(`/vms/new?template=${encodeURIComponent(t.id)}`)}><Icon name="plus" size={14} /><span class="hide-sm">New VM</span></button>
            <Menu size="sm" items={[
              { label: 'Edit name and notes', icon: 'settings', onclick: () => (editing = { id: t.id, name: t.name, description: t.description || '', cloudInit: t.cloudInit, os: t.os }) },
              { divider: true },
              { label: 'Delete template', icon: 'trash', danger: true, onclick: () => remove(t) },
            ]} />
          </div>
        </div>
      {/each}
    {/if}
  </div>

  {#if data}<p class="dim tip">Template images are kept in <code>{data.dir}</code>. Change the folder in <a href="/settings">Settings</a> → Stacks and VMs.</p>{/if}
</div>

{#if adding}
  <AddTemplate start={adding} catalog={data?.catalog || []} onclose={() => (adding = null)} onchange={load} />
{/if}

{#if editing}
  <Modal title="Edit template" onclose={() => (editing = null)} width={520}>
    <form id="tplform" class="stack-v" onsubmit={save}>
      <label class="field"><span>Name</span><input bind:value={editing.name} required maxlength="80" /></label>
      <label class="field"><span>Notes</span><textarea rows="3" bind:value={editing.description} maxlength="500" placeholder="What's installed, when it was last updated…"></textarea></label>
      {#if editing.os === 'linux'}
        <label class="chk"><input type="checkbox" bind:checked={editing.cloudInit} /> <span>Uses cloud-init (new VMs get a first-boot setup disk)</span></label>
      {/if}
    </form>
    {#snippet footer()}
      <button onclick={() => (editing = null)}>Cancel</button>
      <button class="primary" type="submit" form="tplform" disabled={saving}>Save</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .back { display: inline-flex; align-items: center; gap: 5px; color: var(--text-2); font-size: 13px; margin-bottom: 12px; }
  .intro { display: flex; gap: 12px; padding: 14px 18px; margin-bottom: 16px; }
  .intro > :global(svg) { color: var(--accent); flex: none; margin-top: 2px; }
  .intro p { margin: 0; font-size: 13.5px; line-height: 1.55; }
  .intro ul { margin: 6px 0; padding-left: 18px; font-size: 13.5px; line-height: 1.55; }
  .grow { flex: 1; min-width: 0; }
  .err { display: flex; gap: 8px; align-items: center; padding: 12px 16px; color: var(--red-text); margin-bottom: 14px; }
  .busy { margin-bottom: 16px; }
  .trow { display: flex; align-items: center; gap: 14px; padding: 14px 16px 14px 18px; border-bottom: 1px solid var(--border); }
  .trow:last-child { border-bottom: 0; }
  .ico { width: 38px; height: 38px; border-radius: 10px; display: grid; place-items: center; background: var(--surface-2); color: var(--text-3); flex: none; }
  .ico.on { background: var(--accent-soft); color: var(--accent-text); }
  .main { flex: 1; min-width: 0; display: grid; gap: 4px; }
  .main strong { font-size: 14.5px; }
  .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .desc { font-size: 13px; color: var(--text-2); }
  .red-text { color: var(--red-text); }
  .meta { display: flex; gap: 12px; font-size: 12.5px; color: var(--text-2); flex-wrap: wrap; align-items: center; }
  .meter { height: 6px; border-radius: 3px; background: var(--surface-2); overflow: hidden; max-width: 420px; }
  .meter div { height: 100%; background: var(--accent); transition: width 0.4s; }
  .acts { display: flex; gap: 4px; align-items: center; }
  .src { display: inline-flex; align-items: center; gap: 4px; }
  .tip { margin-top: 14px; font-size: 13px; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .chk { display: flex; gap: 8px; align-items: center; font-size: 13.5px; cursor: pointer; }
</style>
