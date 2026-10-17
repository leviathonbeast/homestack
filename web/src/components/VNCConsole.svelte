<script>
  // A VM's screen in the browser (noVNC, loaded on first use). Homestack
  // proxies the VNC connection, so the VM's VNC port never has to be exposed.
  import { onMount } from 'svelte';
  import Icon from '../lib/Icon.svelte';

  let { name } = $props();
  let host = $state();
  let wrap = $state();
  let rfb;
  let status = $state('connecting'); // connecting | connected | password | disconnected
  let message = $state('');
  let password = $state('');
  let scale = $state(true);
  let attempt = $state(0);

  function connect(RFB) {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const url = `${proto}://${location.host}/api/vms/${encodeURIComponent(name)}/console`;
    status = 'connecting';
    message = '';
    rfb = new RFB(host, url, { wsProtocols: ['binary'] });
    rfb.scaleViewport = scale;
    rfb.resizeSession = false;
    rfb.background = '#0f1216';
    rfb.focusOnClick = true;
    rfb.addEventListener('connect', () => {
      status = 'connected';
      rfb.focus();
    });
    rfb.addEventListener('disconnect', (e) => {
      status = 'disconnected';
      message = e.detail.clean ? 'The console closed.' : "Couldn't connect to the console. Is the VM running?";
    });
    rfb.addEventListener('credentialsrequired', () => (status = 'password'));
    rfb.addEventListener('securityfailure', (e) => {
      status = 'disconnected';
      message = e.detail.reason || 'The console password was rejected.';
    });
  }

  let RFBClass;
  onMount(() => {
    let disposed = false;
    import('@novnc/novnc').then(({ default: RFB }) => {
      if (disposed) return;
      RFBClass = RFB;
      connect(RFB);
    });
    return () => {
      disposed = true;
      try {
        rfb?.disconnect();
      } catch {}
    };
  });

  export function reconnect() {
    try {
      rfb?.disconnect();
    } catch {}
    host.innerHTML = '';
    attempt++;
    if (RFBClass) connect(RFBClass);
  }
  function sendPassword(e) {
    e.preventDefault();
    rfb?.sendCredentials({ password });
    password = '';
    status = 'connecting';
  }
  function toggleScale() {
    scale = !scale;
    if (rfb) rfb.scaleViewport = scale;
  }
  function fullscreen() {
    if (document.fullscreenElement) document.exitFullscreen();
    else wrap?.requestFullscreen?.();
  }
</script>

<div class="console" bind:this={wrap}>
  <div class="bar">
    <span class="dot" class:green={status === 'connected'} class:amber={status === 'connecting' || status === 'password'}></span>
    <span class="st">{status === 'connected' ? 'Connected' : status === 'connecting' ? 'Connecting…' : status === 'password' ? 'Password needed' : 'Disconnected'}</span>
    <span class="spacer"></span>
    <button class="sm" disabled={status !== 'connected'} onclick={() => rfb?.sendCtrlAltDel()} title="Send Ctrl+Alt+Del">Ctrl+Alt+Del</button>
    <button class="sm" class:on={scale} onclick={toggleScale} title="Fit the screen to the window"><Icon name="monitor" size={14} /> Fit</button>
    <button class="sm icon" onclick={fullscreen} title="Full screen" aria-label="Full screen"><Icon name="external" size={14} /></button>
    <button class="sm icon" onclick={reconnect} title="Reconnect" aria-label="Reconnect"><Icon name="refresh" size={14} /></button>
  </div>
  <div class="screen">
    <div class="host" bind:this={host}></div>
    {#if status === 'password'}
      <form class="overlay" onsubmit={sendPassword}>
        <Icon name="lock" size={22} />
        <p>This VM's console has a password.</p>
        <!-- svelte-ignore a11y_autofocus -->
        <input type="password" bind:value={password} placeholder="Console password" autofocus />
        <button class="primary" type="submit">Connect</button>
      </form>
    {:else if status === 'disconnected'}
      <div class="overlay">
        <Icon name="monitor" size={22} />
        <p>{message}</p>
        <button class="primary" onclick={reconnect}><Icon name="refresh" size={15} /> Reconnect</button>
      </div>
    {/if}
  </div>
</div>

<style>
  .console { display: flex; flex-direction: column; height: 100%; min-height: 420px; border: 1px solid var(--border); border-radius: 10px; overflow: hidden; background: #0f1216; }
  .bar { display: flex; align-items: center; gap: 8px; padding: 7px 10px; background: var(--surface); border-bottom: 1px solid var(--border); }
  .st { font-size: 13px; color: var(--text-2); }
  .spacer { flex: 1; }
  button.on { background: var(--accent-soft); color: var(--accent-text); border-color: color-mix(in srgb, var(--accent) 35%, var(--border)); }
  .screen { position: relative; flex: 1; min-height: 0; }
  .host { position: absolute; inset: 0; }
  .overlay { position: absolute; inset: 0; display: grid; place-content: center; justify-items: center; gap: 12px; color: #c8ccd4; text-align: center; background: rgb(15 18 22 / 0.85); }
  .overlay input { width: 240px; }
  .console:fullscreen { border-radius: 0; }
</style>
