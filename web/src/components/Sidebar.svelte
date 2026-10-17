<script>
  import Icon from '../lib/Icon.svelte';
  import { route } from '../lib/router.svelte.js';
  import { session, theme, applyTheme, live, features } from '../lib/state.svelte.js';
  import { api } from '../lib/api.js';

  let { open = $bindable(false), updates = 0 } = $props();

  const nav = $derived([
    { href: '/', label: 'Dashboard', icon: 'dashboard' },
    { href: '/stacks', label: 'Stacks', icon: 'layers' },
    { href: '/containers', label: 'Containers', icon: 'box' },
    features.vms ? { href: '/vms', label: 'VMs', icon: 'vm' } : null,
    features.host ? { href: '/storage', label: 'Storage', icon: 'disk' } : null,
    features.host ? { href: '/shares', label: 'Shares', icon: 'share' } : null,
    features.host ? { href: '/files', label: 'Files', icon: 'folder' } : null,
    features.host ? { href: '/fans', label: 'Fans', icon: 'fan' } : null,
    features.host ? { href: '/schedules', label: 'Schedules', icon: 'clock' } : null,
    { href: '/images', label: 'Images', icon: 'disc' },
    { href: '/volumes', label: 'Volumes', icon: 'database' },
    { href: '/networks', label: 'Networks', icon: 'network' },
  ].filter(Boolean));

  function active(href) {
    return href === '/' ? route.path === '/' : route.path === href || route.path.startsWith(href + '/');
  }

  const themes = ['system', 'light', 'dark'];
  const themeIcon = { system: 'monitor', light: 'sun', dark: 'moon' };
  function cycleTheme() {
    const next = themes[(themes.indexOf(theme.value) + 1) % themes.length];
    applyTheme(next);
    api.put('/settings/general', { theme: next }).catch(() => {});
  }

  async function logout() {
    await api.post('/logout').catch(() => {});
    session.authenticated = false;
  }

  // Close the mobile drawer after navigating.
  $effect(() => {
    route.path;
    open = false;
  });
</script>

<aside class:open>
  <a class="brand" href="/">
    <svg width="26" height="26" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="8" fill="var(--accent)"/><path d="M9 12.5 16 9l7 3.5-7 3.5z" fill="var(--on-accent)"/><path d="m9 16.5 7 3.5 7-3.5" fill="none" stroke="var(--on-accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" opacity=".85"/><path d="m9 20.5 7 3.5 7-3.5" fill="none" stroke="var(--on-accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" opacity=".6"/></svg>
    <span>Homestack</span>{#if session.version}<small class="ver">v{session.version}</small>{/if}
  </a>

  <nav>
    {#each nav as item}
      <a href={item.href} class:active={active(item.href)}>
        <Icon name={item.icon} size={17} />
        <span>{item.label}</span>
        {#if item.href === '/stacks' && updates}
          <span class="count" title="{updates} with updates">{updates}</span>
        {/if}
      </a>
    {/each}
  </nav>

  <div class="bottom">
    <a href="/settings" class:active={active('/settings')}>
      <Icon name="settings" size={17} /><span>Settings</span>
    </a>
    <div class="user">
      <div class="avatar">{(session.username || '?').slice(0, 1).toUpperCase()}</div>
      <div class="who">
        <strong class="truncate">{session.username}</strong>
        <span class="conn" title={live.connected ? 'Receiving live updates from Docker' : 'Live updates paused'}>
          <span class="dot" class:green={live.connected}></span>{live.connected ? 'Live' : 'Offline'}
        </span>
      </div>
      <button class="ghost icon sm" onclick={cycleTheme} title="Theme: {theme.value}" aria-label="Change theme">
        <Icon name={themeIcon[theme.value]} size={16} />
      </button>
      <button class="ghost icon sm" onclick={logout} title="Sign out" aria-label="Sign out">
        <Icon name="logout" size={16} />
      </button>
    </div>
  </div>
</aside>
{#if open}<div class="scrim" role="presentation" onclick={() => (open = false)}></div>{/if}

<style>
  aside {
    position: fixed; top: 0; left: 0; bottom: 0; width: var(--sidebar-w); z-index: 60;
    display: flex; flex-direction: column; gap: 4px; padding: 16px 12px;
    background: var(--surface); border-right: 1px solid var(--border);
  }
  .brand { display: flex; align-items: center; gap: 10px; padding: 4px 8px 18px; color: var(--text); font-weight: 650; font-size: 16px; letter-spacing: -0.02em; }
  .brand:hover { text-decoration: none; }
  .ver { font-size: 11px; font-weight: 500; color: var(--text-3); letter-spacing: 0; margin-left: -4px; align-self: flex-end; padding-bottom: 2px; }
  nav, .bottom { display: grid; gap: 2px; }
  nav a, .bottom > a {
    display: flex; align-items: center; gap: 11px; padding: 8px 10px; border-radius: 8px;
    color: var(--text-2); font-weight: 500; font-size: 14px; transition: background 0.1s, color 0.1s;
  }
  nav a:hover, .bottom > a:hover { background: var(--surface-2); color: var(--text); text-decoration: none; }
  nav a.active, .bottom > a.active { background: var(--accent-soft); color: var(--accent-text); }
  .count { margin-left: auto; min-width: 20px; height: 20px; padding: 0 6px; border-radius: 999px; background: var(--amber-soft); color: var(--amber-text); font-size: 11.5px; font-weight: 600; display: grid; place-items: center; }
  .bottom { margin-top: auto; border-top: 1px solid var(--border); padding-top: 10px; }
  .user { display: flex; align-items: center; gap: 8px; padding: 8px 4px 0 6px; }
  .avatar { width: 30px; height: 30px; border-radius: 50%; background: var(--accent-soft); color: var(--accent-text); display: grid; place-items: center; font-weight: 600; font-size: 13px; flex: none; }
  .who { display: grid; min-width: 0; flex: 1; line-height: 1.25; }
  .who strong { font-size: 13px; font-weight: 600; }
  .conn { display: flex; align-items: center; gap: 6px; font-size: 11.5px; color: var(--text-3); }
  .conn .dot { width: 6px; height: 6px; box-shadow: none; }
  .scrim { display: none; }
  @media (max-width: 860px) {
    aside { transform: translateX(-100%); transition: transform 0.2s ease; box-shadow: var(--shadow-lg); }
    aside.open { transform: none; }
    .scrim { display: block; position: fixed; inset: 0; z-index: 55; background: rgb(10 12 16 / 0.4); }
  }
</style>
