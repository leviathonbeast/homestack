<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import HostNetwork from '../components/net/HostNetwork.svelte';
  import VMNetworks from '../components/net/VMNetworks.svelte';
  import DockerNetworks from '../components/net/DockerNetworks.svelte';
  import NetChange from '../components/net/NetChange.svelte';
  import PendingBar from '../components/net/PendingBar.svelte';

  let { tab = 'host' } = $props();

  const tabs = [
    { id: 'host', label: 'Server', icon: 'network', hint: "The server's own ports, bonds, bridges and VLANs" },
    { id: 'vms', label: 'VMs', icon: 'vm', hint: 'Bridges and virtual networks for VMs' },
    { id: 'docker', label: 'Docker', icon: 'layers', hint: 'Networks for containers' },
  ];
  const current = $derived(tabs.some((t) => t.id === tab) ? tab : 'host');

  // The host view is shared by the Server and VMs tabs.
  let host = $state(null);
  let hostError = $state('');
  let unreachable = $state(false);
  let reviewing = $state(null); // an op being reviewed
  let lastSeen = '';

  async function loadHost() {
    try {
      const v = await api.get('/network/host');
      host = v;
      hostError = '';
      unreachable = false;
      // Say what happened to a change (undone after a failed test, the
      // timer running out, an agent restart), even after a reconnect.
      if (v.last && v.last.time !== lastSeen) {
        if (lastSeen && v.last.level === 'warning') toast(v.last.text, 'warn', 10000);
        lastSeen = v.last.time;
      }
    } catch (e) {
      if (e.status === 503) hostError = e.message;
      else if (host?.pending) unreachable = true;
      else hostError = e.message;
    }
  }
  onMount(() => {
    loadHost();
    const t = setInterval(() => {
      if (current !== 'docker' || host?.pending) loadHost();
    }, 4000);
    return () => clearInterval(t);
  });

  function goTab(id) {
    navigate(id === 'host' ? '/networks' : `/networks/${id}`, { replace: true });
  }
  function done() {
    reviewing = null;
    loadHost();
  }
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Networks</h1>
      <p class="sub">How the server, its VMs and its containers connect.</p>
    </div>
  </div>

  {#if host?.pending}
    <PendingBar pending={host.pending} {unreachable} onchange={loadHost} />
  {:else if host?.busy}
    <div class="busy card"><Icon name="loader" size={16} class="spin" /> Applying “{host.busy}” and testing the connections…</div>
  {/if}

  <nav class="tabs">
    {#each tabs as t}
      <a href={t.id === 'host' ? '/networks' : `/networks/${t.id}`} class:active={current === t.id} title={t.hint}
        onclick={(e) => { e.preventDefault(); goTab(t.id); }}><Icon name={t.icon} size={15} /> {t.label}</a>
    {/each}
  </nav>

  {#if current === 'host'}
    {#if host}
      <HostNetwork view={host} onreview={(op) => (reviewing = op)} />
    {:else if hostError}
      <div class="card empty"><h3>The server's network isn't available</h3><p>{hostError}</p></div>
    {:else}
      <div class="skeleton" style="height:320px"></div>
    {/if}
  {:else if current === 'vms'}
    <VMNetworks {host} {hostError} onreview={(op) => (reviewing = op)} />
  {:else}
    <DockerNetworks />
  {/if}
</div>

{#if reviewing}
  <NetChange op={reviewing} onclose={() => (reviewing = null)} ondone={done} />
{/if}

<style>
  .busy { display: flex; gap: 10px; align-items: center; padding: 12px 16px; margin-bottom: 18px; font-size: 13.5px; }
</style>
