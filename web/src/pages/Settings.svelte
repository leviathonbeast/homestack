<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { session, toast, confirm, applyTheme, theme, features } from '../lib/state.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import { ago, dateTime, sourceUrl } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from '../components/Modal.svelte';
  import NotificationSettings from '../components/NotificationSettings.svelte';
  import ZfsArc from '../components/ZfsArc.svelte';
  import StorageLayout from '../components/StorageLayout.svelte';
  import ParitySettings from '../components/ParitySettings.svelte';
  import SpinDown from '../components/SpinDown.svelte';
  import BackupSettings from '../components/BackupSettings.svelte';

  let { section = 'access' } = $props();

  const sections = $derived([
    { id: 'access', label: 'Access', icon: 'shield', desc: 'Who can reach Homestack' },
    { id: 'stacks', label: 'Stacks and VMs', icon: 'layers', desc: 'Folders, app links, updates' },
    features.host ? { id: 'storage', label: 'Storage', icon: 'disk', desc: 'Mover, parity, spin down, paths' } : null,
    { id: 'backups', label: 'Backups', icon: 'archive', desc: 'Download and restore' },
    { id: 'notifications', label: 'Notifications', icon: 'bell', desc: 'Alerts to Discord and ntfy' },
    { id: 'account', label: 'Account', icon: 'user', desc: 'Username, password, devices' },
    { id: 'appearance', label: 'Appearance', icon: 'palette', desc: 'Theme' },
    { id: 'about', label: 'System', icon: 'info', desc: 'Checks, version, recovery' },
  ].filter(Boolean));

  let general = $state(null);
  async function loadGeneral() {
    general = await api.get('/settings');
  }
  onMount(loadGeneral);

  async function saveGeneral(patch, msg = 'Saved') {
    try {
      general = await api.put('/settings/general', patch);
      toast(msg, 'success', 2000);
      return true;
    } catch (e) {
      toast(e.message, 'error');
      return false;
    }
  }

  // ---------- Access ----------
  let access = $state(null);
  let nets = $state([]);
  let listenAll = $state(true);
  let listenSel = $state({});
  let newNet = $state({ name: '', cidr: '' });
  let pending = $state(null); // { token, seconds }
  let countdown = $state(0);
  let accessError = $state('');
  let timer;

  async function loadAccess() {
    access = await api.get('/settings/access');
    nets = access.allowedNetworks.map((n) => ({ ...n }));
    listenAll = !access.listenAddrs?.length;
    listenSel = Object.fromEntries((access.listenAddrs || []).map((a) => [a, true]));
  }
  $effect(() => {
    if (section === 'access') loadAccess().catch((e) => toast(e.message, 'error'));
  });

  const accessDirty = $derived.by(() => {
    if (!access) return false;
    const a = JSON.stringify(access.allowedNetworks.map((n) => [n.name, n.cidr]));
    const b = JSON.stringify(nets.map((n) => [n.name, n.cidr]));
    const la = [...(access.listenAddrs || [])].sort().join(',');
    const lb = listenAll ? '' : Object.keys(listenSel).filter((k) => listenSel[k]).sort().join(',');
    return a !== b || la !== lb;
  });

  function addNet(e) {
    e.preventDefault();
    if (!newNet.cidr.trim()) return;
    nets.push({ name: newNet.name.trim() || newNet.cidr.trim(), cidr: newNet.cidr.trim() });
    newNet = { name: '', cidr: '' };
  }
  function addMine() {
    const ip = access.clientIP;
    const cidr = ip.includes(':') ? `${ip}/128` : ip.replace(/\.\d+$/, '.0/24');
    nets.push({ name: 'My network', cidr });
  }

  async function saveAccess() {
    accessError = '';
    const listenAddrs = listenAll ? [] : Object.keys(listenSel).filter((k) => listenSel[k]);
    if (!listenAll && !listenAddrs.length) {
      accessError = 'Pick at least one address to listen on, or choose all interfaces.';
      return;
    }
    try {
      const r = await api.put('/settings/access', { allowedNetworks: nets, listenAddrs });
      pending = r;
      countdown = r.seconds;
      clearInterval(timer);
      timer = setInterval(() => {
        countdown--;
        if (countdown <= 0) {
          clearInterval(timer);
          pending = null;
          toast('The change was undone because it wasn’t confirmed.', 'warn', 8000);
          setTimeout(() => loadAccess().catch(() => {}), 1000);
        }
      }, 1000);
    } catch (e) {
      accessError = e.message;
    }
  }
  async function keepAccess() {
    try {
      await api.post('/settings/access/confirm', { token: pending.token });
      clearInterval(timer);
      pending = null;
      toast('Access settings saved', 'success');
      loadAccess();
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
  }
  async function undoAccess() {
    clearInterval(timer);
    pending = null;
    await api.post('/settings/access/cancel').catch(() => {});
    toast('Change undone', 'info');
    setTimeout(() => loadAccess().catch(() => {}), 800);
  }
  const kindLabel = { lan: 'LAN', vpn: 'VPN', docker: 'Docker', loopback: 'This machine', other: '' };

  // ---------- Stacks ----------
  let stacksDir = $state('');
  let vmDomainsDir = $state('');
  let vmIsosDir = $state('');
  let vmTemplatesDir = $state('');
  $effect(() => {
    if (general) {
      stacksDir = general.stacksDir;
      vmDomainsDir = general.vmDomainsDir;
      vmIsosDir = general.vmIsosDir;
      vmTemplatesDir = general.vmTemplatesDir;
    }
  });
  let updInfo = $state(null);
  $effect(() => {
    if (section === 'stacks') api.get('/updates').then((u) => (updInfo = u)).catch(() => {});
  });

  // App links: the address that replaces [HOST]. The server's own addresses
  // are offered as suggestions.
  let appHost = $state('');
  let hostAddrs = $state([]);
  $effect(() => {
    if (general) appHost = general.appHost || '';
  });
  $effect(() => {
    if (section !== 'stacks') return;
    api.get('/settings/access')
      .then((a) => (hostAddrs = a.interfaces.filter((i) => i.kind === 'lan' || i.kind === 'vpn').flatMap((i) => i.addrs.map((addr) => ({ addr, name: i.name })))))
      .catch(() => {});
  });
  async function saveAppHost(e) {
    e.preventDefault();
    if (await saveGeneral({ appHost: appHost.trim() }, appHost.trim() ? 'App links now use ' + appHost.trim() : 'App links use the address you opened Homestack on')) {
      session.appHost = general.appHost || '';
    }
  }

  // ---------- Storage (host agent) ----------
  let mover = $state(null);
  let moverAge = $state({ n: 24, unit: 60 });
  function splitAge(min) {
    const unit = min % 1440 === 0 ? 1440 : min % 60 === 0 ? 60 : 1;
    return { n: min / unit, unit };
  }
  async function loadMover() {
    try {
      mover = await api.get('/host/mover');
      moverAge = splitAge(mover.ageMinutes);
    } catch (e) {
      toast(e.message, 'error');
    }
  }
  $effect(() => {
    if (section === 'storage') loadMover();
  });
  const moverMinutes = $derived(Math.round(+moverAge.n * moverAge.unit));
  function ageText(min) {
    const { n, unit } = splitAge(min);
    const word = { 1: 'minute', 60: 'hour', 1440: 'day' }[unit];
    return `${n} ${word}${n === 1 ? '' : 's'}`;
  }
  async function saveMover(minutes) {
    if (minutes !== 0 && !(minutes >= 1 && minutes <= 43200)) {
      toast('Pick an age between 1 minute and 30 days.', 'error');
      return;
    }
    try {
      mover = await api.put('/host/mover', { ageMinutes: minutes });
      moverAge = splitAge(mover.ageMinutes);
      toast(`The mover now moves files older than ${ageText(mover.ageMinutes)}`, 'success', 3000);
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
  }

  // ---------- Account ----------
  let newUsername = $state('');
  let pw = $state({ current: '', next: '', confirm: '' });
  let pwError = $state('');
  let sessions = $state([]);
  $effect(() => {
    if (general) newUsername = general.username;
  });
  async function loadSessions() {
    sessions = await api.get('/account/sessions');
  }
  $effect(() => {
    if (section === 'account') loadSessions().catch(() => {});
  });
  async function changeUsername(e) {
    e.preventDefault();
    try {
      const r = await api.post('/account/username', { username: newUsername });
      session.username = r.username;
      toast('Username changed', 'success');
      loadGeneral();
    } catch (err) {
      toast(err.message, 'error');
    }
  }
  async function changePassword(e) {
    e.preventDefault();
    pwError = '';
    if (pw.next !== pw.confirm) {
      pwError = "The new passwords don't match.";
      return;
    }
    try {
      await api.post('/account/password', { current: pw.current, new: pw.next });
      pw = { current: '', next: '', confirm: '' };
      toast('Password changed. Other devices were signed out.', 'success');
      loadSessions();
    } catch (err) {
      pwError = err.message;
    }
  }
  async function revoke(s) {
    await api.del(`/account/sessions/${s.id}`).catch((e) => toast(e.message, 'error'));
    loadSessions();
  }
  async function revokeOthers() {
    const ok = await confirm({ title: 'Sign out other devices?', message: 'Everything except this browser will need to sign in again.', confirmLabel: 'Sign out others' });
    if (!ok) return;
    await api.post('/account/sessions/revoke-others');
    loadSessions();
  }
  function device(ua) {
    if (!ua) return 'Unknown device';
    const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : /curl/.test(ua) ? 'curl' : 'Browser';
    const os = /iPhone|iPad/.test(ua) ? 'iOS' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'macOS' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : '';
    return os ? `${browser} on ${os}` : browser;
  }

  // ---------- System ----------
  let checks = $state(null);
  async function runChecks() {
    checks = null;
    checks = (await api.get('/preflight').catch((e) => ({ checks: [{ id: 'x', label: 'Checks', status: 'fail', detail: e.message }] }))).checks;
  }
  $effect(() => {
    if (section === 'about') runChecks();
  });
  const statusIcon = { ok: 'check-circle', warn: 'alert', fail: 'x-circle' };
</script>

<div class="page">
  <div class="page-header"><div><h1>Settings</h1></div></div>

  <div class="layout">
    <nav class="subnav">
      {#each sections as s}
        <a href="/settings/{s.id}" class:active={section === s.id} onclick={(e) => { e.preventDefault(); navigate(`/settings/${s.id}`, { replace: true }); }}>
          <Icon name={s.icon} size={16} />
          <span><strong>{s.label}</strong><small>{s.desc}</small></span>
        </a>
      {/each}
    </nav>

    <div class="content">
      {#if section === 'access'}
        {#if !access}
          <div class="skeleton" style="height:300px"></div>
        {:else}
          <div class="you">
            <Icon name="shield" size={18} />
            <span>You're connected from <strong class="mono">{access.clientIP}</strong>{#if access.localIP}{' '}through <strong class="mono">{access.localIP}:{access.port}</strong>{/if}. Homestack won't let you save a change that would lock you out.</span>
          </div>

          <section class="card">
            <div class="card-header"><h2>Allowed networks</h2></div>
            <div class="card-body stack-v">
              <p class="muted small">Requests from anywhere else are refused before they reach the sign-in page.</p>
              <div class="nets">
                {#each nets as n, i}
                  <div class="net">
                    <input class="nname" bind:value={n.name} aria-label="Name" />
                    <input class="mono ncidr" bind:value={n.cidr} aria-label="Network range" />
                    <button class="ghost icon sm" aria-label="Remove" onclick={() => nets.splice(i, 1)}><Icon name="x" size={15} /></button>
                  </div>
                {/each}
              </div>
              <form class="net add" onsubmit={addNet}>
                <input class="nname" bind:value={newNet.name} placeholder="Name, e.g. WireGuard" />
                <input class="mono ncidr" bind:value={newNet.cidr} placeholder="10.8.0.0/24" />
                <button type="submit" class="sm"><Icon name="plus" size={14} /> Add</button>
              </form>
              <div><button class="sm ghost" onclick={addMine}><Icon name="plus" size={14} /> Add the network I'm on</button></div>
            </div>
          </section>

          <section class="card">
            <div class="card-header"><h2>Listen on</h2><span class="dim small">port {access.port}</span></div>
            <div class="card-body stack-v">
              <label class="radio"><input type="radio" bind:group={listenAll} value={true} /> <span><strong>All interfaces</strong><small>Simplest. The allowed networks above still apply.</small></span></label>
              <label class="radio"><input type="radio" bind:group={listenAll} value={false} /> <span><strong>Only these addresses</strong><small>For example, just your LAN and WireGuard, so nothing else can even connect.</small></span></label>
              {#if !listenAll}
                <div class="ifaces">
                  {#each access.interfaces as ifc}
                    {#each ifc.addrs as a}
                      <label class="iface">
                        <input type="checkbox" bind:checked={listenSel[a]} />
                        <span class="mono">{ifc.name}</span>
                        <span class="mono dim">{a}</span>
                        {#if kindLabel[ifc.kind]}<span class="badge {ifc.kind === 'vpn' ? 'accent' : ''}">{kindLabel[ifc.kind]}</span>{/if}
                        {#if a === access.localIP}<span class="badge green">You're using this</span>{/if}
                      </label>
                    {/each}
                  {/each}
                </div>
                <p class="hint">An address that isn't up yet (like a VPN at boot) is retried every 10 seconds.</p>
              {/if}
              <div class="listeners">
                {#each access.listeners as l}
                  <span class="badge {l.bound ? 'green' : 'red'}" title={l.error}><span class="dot {l.bound ? 'green' : 'red'}" style="box-shadow:none"></span>{l.addr}{l.bound ? '' : ' (waiting)'}</span>
                {/each}
              </div>
            </div>
          </section>

          {#if accessError}<p class="error-text">{accessError}</p>{/if}
          <div class="row">
            <span class="spacer"></span>
            <button disabled={!accessDirty} onclick={loadAccess}>Discard</button>
            <button class="primary" disabled={!accessDirty} onclick={saveAccess}>Save access settings</button>
          </div>

          <section class="card">
            <div class="card-header"><h2>Sessions</h2></div>
            <div class="card-body">
              <label class="field inline">
                <span>Stay signed in for</span>
                <select value={general?.sessionDays} onchange={(e) => saveGeneral({ sessionDays: +e.target.value })}>
                  {#each [1, 7, 14, 30, 60, 90, 180, 365] as d}<option value={d}>{d} {d === 1 ? 'day' : 'days'}</option>{/each}
                </select>
              </label>
              <p class="hint">Applies when "Keep me signed in" is ticked. Otherwise you're signed out when the browser closes.</p>
            </div>
          </section>
        {/if}
      {:else if section === 'stacks'}
        <section class="card">
          <div class="card-header"><h2>Stacks folder</h2></div>
          <form class="card-body stack-v" onsubmit={(e) => { e.preventDefault(); saveGeneral({ stacksDir }); }}>
            <label class="field">
              <span>Folder</span>
              <div class="row"><input class="mono" bind:value={stacksDir} /><button type="submit" disabled={stacksDir === general?.stacksDir}>Save</button></div>
              <span class="hint">Every subfolder with a compose file shows up as a stack. When running in Docker, mount this folder at the same path inside the container.</span>
            </label>
          </form>
        </section>
        <section class="card">
          <div class="card-header"><h2>Virtual machines</h2></div>
          <form class="card-body stack-v" onsubmit={(e) => { e.preventDefault(); saveGeneral({ vmDomainsDir, vmIsosDir, vmTemplatesDir }); }}>
            <label class="field"><span>Folder for new VM disks</span><input class="mono" bind:value={vmDomainsDir} /></label>
            <label class="field"><span>Folder with installer ISOs</span><input class="mono" bind:value={vmIsosDir} />
              <span class="hint">When running in Docker, mount this folder into the container at the same path so ISOs can be listed.</span></label>
            <label class="field"><span>Folder for VM templates</span><input class="mono" bind:value={vmTemplatesDir} />
              <span class="hint">Where template disk images are kept. Existing templates stay where they are if you change it.</span></label>
            <div><button type="submit" disabled={vmDomainsDir === general?.vmDomainsDir && vmIsosDir === general?.vmIsosDir && vmTemplatesDir === general?.vmTemplatesDir}>Save</button></div>
          </form>
        </section>
        <section class="card">
          <div class="card-header"><h2>App links</h2></div>
          <form class="card-body stack-v" onsubmit={saveAppHost}>
            <label class="field">
              <span>Address for app links</span>
              <div class="row">
                <input class="mono" bind:value={appHost} placeholder={location.hostname} list="host-addrs" autocapitalize="off" spellcheck="false" />
                <button type="submit" disabled={appHost.trim() === (general?.appHost || '')}>Save</button>
              </div>
              <datalist id="host-addrs">{#each hostAddrs as h}<option value={h.addr}>{h.name}</option>{/each}</datalist>
              <span class="hint">Links to your apps' web pages (dashboard, Stacks, Containers) use this in place of <code>[HOST]</code>. Leave it empty to use the address you opened Homestack on (now <code>{location.hostname}</code>). Set it when you reach Homestack on a management address your apps don't answer on.</span>
            </label>
          </form>
        </section>
        <section class="card">
          <div class="card-header"><h2>Image updates</h2></div>
          <div class="card-body stack-v">
            <label class="field inline">
              <span>Check for newer images</span>
              <select value={general?.updateCheckHours} onchange={(e) => saveGeneral({ updateCheckHours: +e.target.value })}>
                <option value={0}>Never</option><option value={1}>Every hour</option><option value={6}>Every 6 hours</option>
                <option value={12}>Every 12 hours</option><option value={24}>Daily</option><option value={168}>Weekly</option>
              </select>
            </label>
            <p class="hint">Homestack only compares versions with the registry; it never updates anything on its own. {#if updInfo?.lastRun}Last checked {ago(updInfo.lastRun)}.{/if}</p>
            <label class="toggle">
              <input type="checkbox" checked={general?.pruneAfterUpdate} onchange={(e) => saveGeneral({ pruneAfterUpdate: e.target.checked })} />
              <span><strong>Remove old images after updating a stack</strong><small>Frees disk space. You won't be able to roll back to the previous image without pulling it again.</small></span>
            </label>
          </div>
        </section>
      {:else if section === 'storage'}
        <section class="card">
          <div class="card-header"><h2>Mover</h2></div>
          {#if !mover}
            <div class="card-body"><div class="skeleton" style="height:90px"></div></div>
          {:else}
            <form class="card-body stack-v" onsubmit={(e) => { e.preventDefault(); saveMover(moverMinutes); }}>
              <div class="field">
                <span>Move files older than</span>
                <div class="row">
                  <input type="number" min="1" step="1" bind:value={moverAge.n} style="max-width:110px" aria-label="Age" />
                  <select bind:value={moverAge.unit} style="width:auto" aria-label="Unit">
                    <option value={1}>{+moverAge.n === 1 ? 'minute' : 'minutes'}</option><option value={60}>{+moverAge.n === 1 ? 'hour' : 'hours'}</option><option value={1440}>{+moverAge.n === 1 ? 'day' : 'days'}</option>
                  </select>
                  <button type="submit" disabled={moverMinutes === mover.ageMinutes}>Save</button>
                </div>
                <span class="hint">Newer files stay on the cache, so recent downloads and anything still being written aren't moved yet. When the mover runs is set on <a href="/schedules">Schedules</a>.</span>
              </div>
              {#if mover.custom && mover.defaultMinutes !== mover.ageMinutes}
                <p class="hint">Without this setting the server's alerts file (<code>/etc/default/homestack-alerts</code>) says {ageText(mover.defaultMinutes)}. <button type="button" class="linkish" onclick={() => saveMover(0)}>Use that instead</button></p>
              {/if}
            </form>
          {/if}
        </section>
        <ParitySettings />
        <SpinDown />
        <ZfsArc />
        <StorageLayout />
      {:else if section === 'backups'}
        <BackupSettings />
      {:else if section === 'notifications'}
        <NotificationSettings />
      {:else if section === 'account'}
        <section class="card">
          <div class="card-header"><h2>Username</h2></div>
          <form class="card-body row" onsubmit={changeUsername}>
            <input bind:value={newUsername} autocomplete="username" style="max-width:280px" />
            <button type="submit" disabled={newUsername === general?.username}>Save</button>
          </form>
        </section>
        <section class="card">
          <div class="card-header"><h2>Password</h2></div>
          <form class="card-body stack-v pwform" onsubmit={changePassword}>
            <label class="field"><span>Current password</span><input type="password" bind:value={pw.current} autocomplete="current-password" required /></label>
            <label class="field"><span>New password</span><input type="password" bind:value={pw.next} autocomplete="new-password" minlength="8" required /></label>
            <label class="field"><span>Confirm new password</span><input type="password" bind:value={pw.confirm} autocomplete="new-password" required /></label>
            {#if pwError}<p class="error-text">{pwError}</p>{/if}
            <div><button type="submit" class="primary">Change password</button></div>
          </form>
        </section>
        <section class="card">
          <div class="card-header"><h2>Signed-in devices</h2><span class="spacer"></span>
            {#if sessions.length > 1}<button class="sm" onclick={revokeOthers}>Sign out others</button>{/if}</div>
          {#each sessions as s (s.id)}
            <div class="sess">
              <Icon name="monitor" size={18} />
              <div class="grow">
                <strong>{device(s.userAgent)}</strong>{#if s.current}<span class="badge green tiny">This device</span>{/if}
                <div class="dim small">{s.ip} · active {ago(s.lastSeen)} · expires {dateTime(s.expires)}</div>
              </div>
              {#if !s.current}<button class="sm ghost" onclick={() => revoke(s)}>Sign out</button>{/if}
            </div>
          {/each}
        </section>
      {:else if section === 'appearance'}
        <section class="card">
          <div class="card-header"><h2>Theme</h2></div>
          <div class="card-body">
            <div class="themes">
              {#each [['system', 'System', 'monitor'], ['light', 'Light', 'sun'], ['dark', 'Dark', 'moon']] as [v, l, i]}
                <button class="themebtn" class:active={theme.value === v} onclick={() => { applyTheme(v); saveGeneral({ theme: v }, 'Theme saved'); }}>
                  <Icon name={i} size={20} /> {l}
                </button>
              {/each}
            </div>
          </div>
        </section>
      {:else if section === 'about'}
        <section class="card">
          <div class="card-header"><h2>System checks</h2><span class="spacer"></span><button class="sm" onclick={runChecks}><Icon name="refresh" size={14} /> Run again</button></div>
          {#if !checks}
            <div class="card-body"><div class="skeleton" style="height:120px"></div></div>
          {:else}
            {#each checks as c (c.id)}
              <div class="check {c.status}">
                <Icon name={statusIcon[c.status]} size={17} />
                <div class="grow"><strong>{c.label}</strong><div class="muted small">{c.detail}</div>
                  {#if c.fix}<pre class="fix">{c.fix}</pre>{/if}</div>
              </div>
            {/each}
          {/if}
        </section>
        <section class="card">
          <div class="card-header"><h2>About</h2></div>
          <dl class="card-body about">
            <dt>Version</dt><dd>{general?.version}</dd>
            {#if general?.source}<dt>Source code</dt><dd><a href={sourceUrl(general.source, general.version)} target="_blank" rel="noopener">{general.source.replace(/^https?:\/\//, '')}</a></dd>{/if}
            <dt>Licence</dt><dd>Free software under the <a href="https://www.gnu.org/licenses/agpl-3.0.html" target="_blank" rel="noopener">GNU AGPL</a>, version 3 or later. The libraries it includes keep their own: <a href="/third-party-notices.txt" target="_blank" rel="noopener">third-party licences</a>.</dd>
            <dt>Settings file</dt><dd class="mono">{general?.stateFile}</dd>
          </dl>
        </section>
        <section class="card">
          <div class="card-header"><h2>If you get locked out</h2></div>
          <div class="card-body stack-v small">
            <p class="muted">Run these on the server. A running Homestack picks up the change within a few seconds.</p>
            <div><strong>Allow every private network again</strong><pre class="fix">homestack reset-access
# or, in Docker:
docker exec homestack homestack reset-access</pre></div>
            <div><strong>Reset the admin password</strong><pre class="fix">homestack reset-password
# or, in Docker:
docker exec homestack homestack reset-password</pre></div>
          </div>
        </section>
      {/if}
    </div>
  </div>
</div>

{#if pending}
  <Modal title="Keep these access settings?" dismissable={false} width={440}>
    <p>If you can still see this page, the new settings work for you. If you don't confirm, the old settings come back automatically.</p>
    <div class="count">{countdown}s</div>
    {#snippet footer()}
      <button onclick={undoAccess}>Undo</button>
      <button class="primary" onclick={keepAccess}>Keep settings</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .layout { display: grid; grid-template-columns: 240px minmax(0, 1fr); gap: 28px; align-items: start; }
  .subnav { display: grid; gap: 2px; position: sticky; top: 20px; }
  .subnav a { display: flex; gap: 11px; align-items: flex-start; padding: 9px 11px; border-radius: 9px; color: var(--text-2); }
  .subnav a:hover { background: var(--surface-2); text-decoration: none; }
  .subnav a.active { background: var(--surface); box-shadow: var(--shadow), 0 0 0 1px var(--border); color: var(--text); }
  .subnav a.active :global(svg) { color: var(--accent); }
  .subnav :global(svg) { margin-top: 2px; flex: none; }
  .subnav span { display: grid; }
  .subnav strong { font-size: 13.5px; font-weight: 500; color: var(--text); }
  .subnav small { font-size: 12px; color: var(--text-3); }
  .content { display: grid; gap: 16px; }
  .small { font-size: 13px; }
  .spacer { flex: 1; }
  .grow { flex: 1; min-width: 0; }
  .you { display: flex; gap: 10px; align-items: flex-start; padding: 12px 14px; border-radius: 10px; background: var(--accent-soft); color: var(--accent-text); font-size: 13.5px; }
  .you :global(svg) { flex: none; margin-top: 1px; }
  .nets { display: grid; gap: 6px; }
  .net { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) 34px; gap: 8px; align-items: center; }
  .net.add { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) auto; padding-top: 6px; border-top: 1px dashed var(--border); }
  .radio, .toggle { display: flex; gap: 10px; align-items: flex-start; cursor: pointer; }
  .radio input, .toggle input { margin-top: 3px; width: 16px; height: 16px; accent-color: var(--accent); flex: none; }
  .radio span, .toggle span { display: grid; }
  .radio small, .toggle small { color: var(--text-3); font-size: 12.5px; }
  .ifaces { display: grid; gap: 2px; margin-left: 26px; border: 1px solid var(--border); border-radius: 10px; padding: 6px; }
  .iface { display: flex; gap: 10px; align-items: center; padding: 7px 8px; border-radius: 7px; cursor: pointer; flex-wrap: wrap; }
  .iface:hover { background: var(--surface-2); }
  .listeners { display: flex; gap: 6px; flex-wrap: wrap; }
  .field.inline { display: flex; align-items: center; gap: 14px; }
  .field.inline select { width: auto; }
  .pwform { max-width: 380px; }
  .sess { display: flex; gap: 12px; align-items: center; padding: 12px 18px; border-bottom: 1px solid var(--border); }
  .sess:last-child { border-bottom: 0; }
  .sess :global(svg) { color: var(--text-3); }
  .tiny { height: 19px; font-size: 11px; margin-left: 8px; }
  .themes { display: grid; grid-template-columns: repeat(3, 1fr); gap: 10px; max-width: 480px; }
  .themebtn { height: 80px; flex-direction: column; gap: 8px; border-radius: 12px; }
  .themebtn.active { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); color: var(--accent-text); }
  .check { display: flex; gap: 12px; padding: 12px 18px; border-bottom: 1px solid var(--border); }
  .check:last-child { border-bottom: 0; }
  .check > :global(svg) { flex: none; margin-top: 2px; }
  .check.ok > :global(svg) { color: var(--green); }
  .check.warn > :global(svg) { color: var(--amber); }
  .check.fail > :global(svg) { color: var(--red); }
  .fix { margin: 8px 0 0; padding: 10px 12px; background: var(--surface-2); border: 1px solid var(--border); border-radius: 8px; white-space: pre-wrap; font-size: 12px; }
  .about { display: grid; grid-template-columns: 130px 1fr; gap: 10px; margin: 0; }
  .about dt { color: var(--text-2); }
  .about dd { margin: 0; overflow-wrap: anywhere; }
  .linkish { all: unset; color: var(--accent-text); cursor: pointer; }
  .linkish:hover { text-decoration: underline; }
  .count { font-size: 40px; font-weight: 600; text-align: center; margin: 14px 0 4px; color: var(--text); font-variant-numeric: tabular-nums; }
  @media (max-width: 860px) {
    .layout { grid-template-columns: 1fr; }
    .subnav { position: static; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); }
    .subnav small { display: none; }
    .net, .net.add { grid-template-columns: 1fr; }
  }
</style>
