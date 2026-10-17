<script>
  import { onMount } from 'svelte';
  import { api, subscribe } from './lib/api.js';
  import { session, live, bumpLive, applyTheme, features, jobs } from './lib/state.svelte.js';
  import { route, match } from './lib/router.svelte.js';
  import Icon from './lib/Icon.svelte';

  import Sidebar from './components/Sidebar.svelte';
  import Toasts from './components/Toasts.svelte';
  import ConfirmHost from './components/ConfirmHost.svelte';
  import JobsPanel from './components/JobsPanel.svelte';

  import Auth from './pages/Auth.svelte';
  import Dashboard from './pages/Dashboard.svelte';
  import Stacks from './pages/Stacks.svelte';
  import StackDetail from './pages/StackDetail.svelte';
  import NewStack from './pages/NewStack.svelte';
  import Containers from './pages/Containers.svelte';
  import ContainerDetail from './pages/ContainerDetail.svelte';
  import Images from './pages/Images.svelte';
  import Volumes from './pages/Volumes.svelte';
  import Networks from './pages/Networks.svelte';
  import Settings from './pages/Settings.svelte';
  import VMs from './pages/VMs.svelte';
  import Storage from './pages/Storage.svelte';
  import ReplaceDisk from './pages/ReplaceDisk.svelte';
  import DiskDetail from './pages/DiskDetail.svelte';
  import Fans from './pages/Fans.svelte';
  import Schedules from './pages/Schedules.svelte';
  import Shares from './pages/Shares.svelte';
  import Files from './pages/Files.svelte';
  import VMDetail from './pages/VMDetail.svelte';
  import NewVM from './pages/NewVM.svelte';
  import VMTemplates from './pages/VMTemplates.svelte';
  import NotFound from './pages/NotFound.svelte';

  const routes = [
    ['/', Dashboard],
    ['/stacks', Stacks],
    ['/stacks/new', NewStack],
    ['/stacks/:name/:tab?', StackDetail],
    ['/containers', Containers],
    ['/containers/:id/:tab?', ContainerDetail],
    ['/images', Images],
    ['/volumes', Volumes],
    ['/networks/:tab?', Networks],
    ['/storage', Storage],
    ['/storage/replace', ReplaceDisk],
    ['/storage/disks/:name/:tab?', DiskDetail],
    ['/fans', Fans],
    ['/schedules', Schedules],
    ['/shares', Shares],
    ['/files', Files],
    ['/vms', VMs],
    ['/vms/new', NewVM],
    ['/vms/templates', VMTemplates],
    ['/vms/:name/:tab?', VMDetail],
    ['/settings/:section?', Settings],
  ];

  const current = $derived.by(() => {
    for (const [pattern, component] of routes) {
      const params = match(pattern, route.path);
      if (params) return { component, params, pattern };
    }
    return { component: NotFound, params: {}, pattern: '' };
  });

  let menuOpen = $state(false);
  let stackUpdates = $state(0);

  onMount(async () => {
    try {
      const s = await api.get('/session');
      Object.assign(session, s, { loaded: true });
      let local = null;
      try {
        local = localStorage.getItem('homestack-theme');
      } catch {}
      applyTheme(local || s.theme || 'system');
    } catch (e) {
      session.loaded = true;
      session.error = e.message;
    }
  });

  // Live updates from Docker, while signed in.
  $effect(() => {
    if (!session.authenticated) return;
    const close = subscribe('/events', {
      ready: () => (live.connected = true),
      docker: () => bumpLive(),
      error: () => (live.connected = false),
    });
    return () => {
      close();
      live.connected = false;
    };
  });

  // Is VM management available on this server? (And the app link address,
  // which signing in doesn't return.)
  $effect(() => {
    if (!session.authenticated) return;
    api.get('/session').then((s) => (session.appHost = s.appHost || '')).catch(() => {});
    api.get('/host/available').then((h) => (features.host = h.available)).catch(() => {});
    api.get('/vms/status')
      .then((s) => Object.assign(features, { vms: s.available, vmsChecked: true }))
      .catch(() => (features.vmsChecked = true));
  });

  // Count stacks with image updates for the sidebar badge.
  $effect(() => {
    if (!session.authenticated) return;
    live.tick;
    route.path;
    api.get('/stacks')
      .then((list) => (stackUpdates = list.filter((s) => s.updateAvailable && !s.external).length))
      .catch(() => {});
  });

  const pageTitle = $derived.by(() => {
    const p = current.params;
    if (p.name) return `${p.name} · Homestack`;
    const base = route.path.split('/')[1];
    return base ? `${base[0].toUpperCase()}${base.slice(1)} · Homestack` : 'Homestack';
  });
</script>

<svelte:head><title>{pageTitle}</title></svelte:head>

{#if !session.loaded}
  <div class="splash"><Icon name="loader" class="spin" size={22} /></div>
{:else if session.error && !session.authenticated}
  <div class="splash"><p>{session.error}</p></div>
{:else if session.setupRequired || !session.authenticated}
  <Auth />
{:else}
  <Sidebar bind:open={menuOpen} updates={stackUpdates} />
  <div class="topbar">
    <button class="ghost icon" onclick={() => (menuOpen = true)} aria-label="Open menu"><Icon name="menu" size={20} /></button>
    <strong>Homestack</strong>
  </div>
  <!-- Room below the content while the output panel is open, so nothing is stuck behind it. -->
  <main class:jobs-room={jobs.open && jobs.list.length > 0}>
    {#key current.pattern + (current.params.name || current.params.id || '')}
      <current.component {...current.params} />
    {/key}
  </main>
  <JobsPanel />
{/if}
<Toasts />
<ConfirmHost />

<style>
  main { margin-left: var(--sidebar-w); min-height: 100vh; }
  main.jobs-room { padding-bottom: 330px; }
  .splash { min-height: 100vh; display: grid; place-items: center; color: var(--text-3); }
  .topbar { display: none; }
  @media (max-width: 860px) {
    main { margin-left: 0; }
    .topbar {
      display: flex; align-items: center; gap: 8px; position: sticky; top: 0; z-index: 40;
      padding: 8px 12px; background: color-mix(in srgb, var(--bg) 85%, transparent);
      backdrop-filter: blur(8px); border-bottom: 1px solid var(--border);
    }
  }
</style>
