<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { bytes, percent } from '../lib/format.js';
  import { vmStateLabel, vmStateColor, vmAction, vmMenu } from '../lib/vms.js';
  import { navigate } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Menu from '../components/Menu.svelte';

  let list = $state(null);
  let error = $state('');

  async function load() {
    try {
      list = await api.get('/vms');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(() => {
    load();
    const t = setInterval(load, 3000);
    return () => clearInterval(t);
  });

  const running = $derived((list || []).filter((v) => v.state === 'running').length);
  const assigned = $derived((list || []).filter((v) => v.state === 'running').reduce((a, v) => a + v.memory, 0));

  function net(vm) {
    return [...new Set(vm.nics.map((n) => n.source))].join(', ');
  }
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Virtual machines</h1>
      <p class="sub">{list ? `${running} of ${list.length} running · ${bytes(assigned)} memory assigned` : ' '}</p>
    </div>
    <div class="hdr-acts">
      <a class="btn" href="/vms/templates"><Icon name="copy" size={16} /> Templates</a>
      <a class="btn primary" href="/vms/new"><Icon name="plus" size={16} /> New VM</a>
    </div>
  </div>

  {#if error}<div class="card err"><Icon name="alert" size={16} /> {error}</div>{/if}

  <div class="card">
    {#if list === null && !error}
      {#each Array(2) as _}<div class="vrow"><div class="skeleton" style="height:18px;width:40%"></div></div>{/each}
    {:else if list && !list.length}
      <div class="empty">
        <h3>No virtual machines yet</h3>
        <p>Create one from an installer ISO, or from a <a href="/vms/templates">template</a> (an official cloud image, ready in seconds).</p>
        <a class="btn primary" href="/vms/new"><Icon name="plus" size={16} /> New VM</a>
      </div>
    {:else}
      {#each list || [] as vm (vm.uuid)}
        {@const on = vm.state === 'running'}
        <div class="vrow" role="link" tabindex="0" onclick={() => navigate(`/vms/${encodeURIComponent(vm.name)}`)} onkeydown={(e) => e.key === 'Enter' && e.target === e.currentTarget && navigate(`/vms/${encodeURIComponent(vm.name)}`)}>
          <div class="ico" class:on><Icon name="vm" size={20} /></div>
          <div class="main">
            <div class="row">
              <strong>{vm.name}</strong>
              <span class="badge {vmStateColor(vm.state)}">{vmStateLabel[vm.state] || vm.state}</span>
              {#if vm.autostart}<span class="badge" title="Starts when the server boots">Autostart</span>{/if}
            </div>
            <div class="meta">
              {#if on && vm.addrs?.length}<span class="addr" title={vm.addrs.join(', ')}>{vm.addrs[0]}{vm.addrs.length > 1 ? ` +${vm.addrs.length - 1}` : ''}</span>{/if}
              <span>{vm.vcpus} vCPU · {bytes(vm.memory, 0)}</span>
              <span class="hide-sm">{vm.firmware === 'uefi' ? 'UEFI' : 'BIOS'}</span>
              {#if vm.nics.length}<span class="hide-sm">{net(vm)}</span>{/if}
              {#if vm.os}<span class="hide-sm dim">{vm.os}</span>{/if}
              {#if vm.template}<span class="hide-sm dim" title="Cloned from this template">from {vm.template}</span>{/if}
            </div>
          </div>
          <div class="usage hide-sm">
            {#if on}
              <div class="u"><span>CPU</span><div class="meter"><div style="width:{Math.min(100, vm.cpu)}%"></div></div><b>{percent(vm.cpu, 0)}</b></div>
              <div class="u" title={vm.guestMem ? "Used by programs in the VM" : "Taken from the server (includes the VM's caches)"}><span>RAM</span><div class="meter"><div style="width:{vm.memory ? Math.min(100, (vm.memUsed / vm.memory) * 100) : 0}%"></div></div><b>{bytes(vm.memUsed, 1)}</b></div>
            {/if}
          </div>
          <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
          <div class="acts" onclick={(e) => e.stopPropagation()}>
            {#if on}
              <button class="sm" onclick={() => navigate(`/vms/${encodeURIComponent(vm.name)}/console`)} disabled={vm.console !== 'vnc'} title={vm.console === 'vnc' ? 'Open console' : 'No VNC console'}><Icon name="terminal" size={14} /><span class="hide-sm">Console</span></button>
            {:else if vm.state === 'paused'}
              <button class="sm" onclick={async () => (await vmAction(vm, 'resume')) && load()}><Icon name="play" size={14} /><span class="hide-sm">Resume</span></button>
            {:else}
              <button class="sm" onclick={async () => (await vmAction(vm, 'start')) && load()}><Icon name="play" size={14} /><span class="hide-sm">Start</span></button>
            {/if}
            <Menu size="sm" items={vmMenu(vm, { onChange: load })} />
          </div>
        </div>
      {/each}
    {/if}
  </div>
</div>

<style>
  .err { display: flex; gap: 8px; align-items: center; padding: 12px 16px; color: var(--red-text); margin-bottom: 14px; }
  .vrow { display: flex; align-items: center; gap: 14px; padding: 14px 16px 14px 18px; border-bottom: 1px solid var(--border); cursor: pointer; transition: background 0.1s; }
  .vrow:last-child { border-bottom: 0; }
  .vrow:hover { background: var(--surface-2); }
  .ico { width: 38px; height: 38px; border-radius: 10px; display: grid; place-items: center; background: var(--surface-2); color: var(--text-3); flex: none; }
  .ico.on { background: var(--accent-soft); color: var(--accent-text); }
  .main { flex: 1; min-width: 0; display: grid; gap: 3px; }
  .main strong { font-size: 14.5px; }
  .meta { display: flex; gap: 12px; font-size: 12.5px; color: var(--text-2); flex-wrap: wrap; }
  .usage { width: 230px; display: grid; gap: 5px; }
  .u { display: grid; grid-template-columns: 32px 1fr 64px; align-items: center; gap: 8px; font-size: 12px; color: var(--text-3); }
  .u b { font-weight: 500; color: var(--text-2); text-align: right; font-variant-numeric: tabular-nums; }
  .acts { display: flex; gap: 4px; align-items: center; }
  .hdr-acts { display: flex; gap: 8px; }
  .addr { font-family: var(--mono, ui-monospace, monospace); color: var(--text); }
</style>
