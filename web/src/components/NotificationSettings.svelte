<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast, confirm } from '../lib/state.svelte.js';
  import { ago } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';

  let info = $state(null);
  let error = $state('');
  let editing = $state(false);
  let url = $state('');
  let serverName = $state('');
  let testing = $state(false);
  let saving = $state(false);
  // ntfy (backup)
  let nt = $state({ server: 'https://ntfy.sh', topic: '', token: '', mode: 'fallback' });
  let ntEditing = $state(false);
  let ntTesting = $state(false);

  async function load() {
    try {
      info = await api.get('/settings/notifications');
      serverName = info.serverName;
      editing = !info.configured;
      resetNtfy();
    } catch (e) {
      error = e.message;
    }
  }
  onMount(load);

  const groups = $derived.by(() => {
    const out = [];
    for (const e of info?.events || []) {
      let g = out.find((x) => x.name === e.group);
      if (!g) out.push((g = { name: e.group, events: [] }));
      g.events.push(e);
    }
    return out;
  });

  async function put(body, msg) {
    try {
      info = await api.put('/settings/notifications', body);
      if (msg) toast(msg, 'success', 2000);
      return true;
    } catch (e) {
      toast(e.message, 'error');
      return false;
    }
  }

  async function test(withUrl) {
    testing = true;
    try {
      await api.post('/settings/notifications/test', withUrl ? { webhook: url } : {});
      toast('Sent. Check your Discord channel.', 'success');
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    testing = false;
    info = await api.get('/settings/notifications').catch(() => info);
  }

  async function save(e) {
    e.preventDefault();
    saving = true;
    if (await put({ webhook: url }, 'Discord connected')) {
      url = '';
      editing = false;
    }
    saving = false;
  }

  async function remove() {
    const ok = await confirm({
      title: 'Stop sending notifications?',
      message: 'Homestack forgets the webhook URL. Your choices below are kept for next time.',
      confirmLabel: 'Remove', danger: true,
    });
    if (ok) await put({ webhook: '' }, 'Notifications turned off');
  }

  function resetNtfy() {
    const n = info.ntfy;
    nt = { server: n.server, topic: n.topic || '', token: '', mode: n.mode };
    ntEditing = !n.configured;
  }
  function randomTopic() {
    const a = new Uint8Array(12);
    crypto.getRandomValues(a);
    const chars = 'abcdefghijkmnpqrstuvwxyz23456789';
    const name = (serverName || info.hostname || 'server').toLowerCase().replace(/[^a-z0-9]+/g, '-').slice(0, 20);
    nt.topic = `${name}-${Array.from(a, (b) => chars[b % chars.length]).join('')}`;
  }
  function ntfyBody() {
    const b = { server: nt.server, topic: nt.topic.trim(), mode: nt.mode };
    if (nt.token.trim()) b.token = nt.token.trim();
    return b;
  }
  async function testNtfy() {
    ntTesting = true;
    try {
      await api.post('/settings/notifications/test', { target: 'ntfy', ntfy: ntEditing ? ntfyBody() : undefined });
      toast('Sent. Check the ntfy app on your phone.', 'success');
    } catch (e) {
      toast(e.message, 'error', 8000);
    }
    ntTesting = false;
    info = await api.get('/settings/notifications').catch(() => info);
  }
  async function saveNtfy(e) {
    e?.preventDefault();
    if (await put({ ntfy: ntfyBody() }, 'ntfy saved')) resetNtfy();
  }
  async function setMode(mode) {
    nt.mode = mode;
    if (info.ntfy.configured && !ntEditing) await put({ ntfy: { server: info.ntfy.server, topic: info.ntfy.topic, mode } }, 'Saved');
  }
  async function removeNtfy() {
    const ok = await confirm({ title: 'Remove the ntfy backup?', message: 'Alerts will only go to Discord.', confirmLabel: 'Remove', danger: true });
    if (ok && (await put({ ntfy: { topic: '' } }, 'ntfy removed'))) resetNtfy();
  }
  const modes = [
    ['fallback', 'Only when Discord fails', 'Discord is tried first (with retries). If it can\'t be reached or refuses the message, it comes here instead.'],
    ['important', 'Warnings and errors too', 'Also sends every warning and error here, so the important ones reach your phone even if Discord notifications are muted.'],
    ['all', 'Everything', 'Every message goes to both at the same time, so a slow or broken Discord never delays your phone.'],
  ];

  const levelIcon = { info: 'info', success: 'check', warning: 'alert', error: 'alert' };
</script>

{#if error}
  <section class="card"><div class="card-body err"><Icon name="alert" size={16} /> {error}</div></section>
{:else if !info}
  <div class="skeleton" style="height:260px"></div>
{:else}
  <section class="card">
    <div class="card-header"><h2>Discord</h2>
      {#if info.configured}<span class="pill ok"><Icon name="check" size={12} /> Connected</span>{/if}
    </div>
    <div class="card-body stack-v">
      {#if info.configured && !editing}
        <div class="row wrap">
          <Icon name="bell" size={16} />
          <span class="grow">Sending to <strong>{info.webhookHint}</strong></span>
          <button class="sm" onclick={() => test(false)} disabled={testing}>
            {#if testing}<Icon name="loader" size={14} class="spin" />{:else}<Icon name="send" size={14} />{/if} Send a test
          </button>
          <button class="sm" onclick={() => (editing = true)}>Replace</button>
          <button class="sm danger" onclick={remove}>Remove</button>
        </div>
      {:else}
        <ol class="steps">
          <li>In Discord, open the channel's settings (<strong>Edit Channel</strong>) → <strong>Integrations</strong> → <strong>Webhooks</strong> → <strong>New Webhook</strong>.</li>
          <li>Click <strong>Copy Webhook URL</strong>.</li>
          <li>Paste it here, send a test, then save.</li>
        </ol>
        <form class="stack-v" onsubmit={save}>
          <label class="field"><span>Webhook URL</span>
            <input class="mono" type="password" bind:value={url} placeholder="https://discord.com/api/webhooks/…" autocomplete="off" spellcheck="false" required />
            <span class="hint">The URL works like a password: anyone with it can post in that channel. Homestack stores it with your other settings and never shows it again.</span>
          </label>
          <div class="row">
            <button type="button" onclick={() => test(true)} disabled={!url.trim() || testing}>
              {#if testing}<Icon name="loader" size={14} class="spin" />{:else}<Icon name="send" size={14} />{/if} Send a test
            </button>
            <button class="primary" type="submit" disabled={!url.trim() || saving}>Save</button>
            {#if info.configured}<button type="button" class="ghost" onclick={() => { editing = false; url = ''; }}>Cancel</button>{/if}
          </div>
        </form>
      {/if}
      <form class="field" onsubmit={(e) => { e.preventDefault(); put({ serverName }, 'Saved'); }}>
        <span>Name shown on messages</span>
        <div class="row"><input bind:value={serverName} placeholder={info.hostname} style="max-width:280px" />
          <button type="submit" disabled={serverName === info.serverName}>Save</button></div>
        <span class="hint">Useful if more than one server posts to the same channel. Leave empty to use the hostname ({info.hostname}).</span>
      </form>
    </div>
  </section>

  <section class="card">
    <div class="card-header"><h2>ntfy (backup)</h2>
      {#if info.ntfy.configured}<span class="pill ok"><Icon name="check" size={12} /> On</span>{/if}
    </div>
    <div class="card-body stack-v">
      <p class="small dim-2">Push notifications to the <a href="https://ntfy.sh" target="_blank" rel="noreferrer">ntfy</a> app on your phone, for when Discord is down, the webhook stops working, or you've muted the channel.</p>
      {#if info.ntfy.configured && !ntEditing}
        <div class="row wrap">
          <Icon name="bell" size={16} />
          <span class="grow">Topic <strong class="mono">{info.ntfy.topic}</strong> on {info.ntfy.server}{#if info.ntfy.tokenHint} · token {info.ntfy.tokenHint}{/if}</span>
          <button class="sm" onclick={testNtfy} disabled={ntTesting}>
            {#if ntTesting}<Icon name="loader" size={14} class="spin" />{:else}<Icon name="send" size={14} />{/if} Send a test
          </button>
          <button class="sm" onclick={() => (ntEditing = true)}>Change</button>
          <button class="sm danger" onclick={removeNtfy}>Remove</button>
        </div>
      {:else}
        <ol class="steps">
          <li>Install the <strong>ntfy</strong> app (Android or iPhone).</li>
          <li>Pick a topic name below (<strong>Generate</strong> makes one nobody can guess: on ntfy.sh, anyone who knows a topic's name can read it).</li>
          <li>In the app, tap <strong>+</strong> and subscribe to that exact topic, then send a test and save.</li>
        </ol>
        <form class="stack-v" onsubmit={saveNtfy}>
          <div class="ntgrid">
            <label class="field"><span>Topic</span>
              <div class="row"><input class="mono grow" bind:value={nt.topic} placeholder="server-…" autocomplete="off" spellcheck="false" required />
                <button type="button" onclick={randomTopic}><Icon name="wand" size={14} /> Generate</button></div>
            </label>
            <label class="field"><span>Server</span>
              <input class="mono" bind:value={nt.server} placeholder="https://ntfy.sh" spellcheck="false" /></label>
            <label class="field"><span>Access token <small class="dim">(optional)</small></span>
              <input class="mono" type="password" bind:value={nt.token} placeholder={info.ntfy.tokenHint ? `saved: ${info.ntfy.tokenHint}` : 'tk_… for a reserved topic'} autocomplete="off" spellcheck="false" /></label>
          </div>
          <div class="row">
            <button type="button" onclick={testNtfy} disabled={!nt.topic.trim() || ntTesting}>
              {#if ntTesting}<Icon name="loader" size={14} class="spin" />{:else}<Icon name="send" size={14} />{/if} Send a test
            </button>
            <button class="primary" type="submit" disabled={!nt.topic.trim()}>Save</button>
            {#if info.ntfy.configured}<button type="button" class="ghost" onclick={resetNtfy}>Cancel</button>{/if}
          </div>
        </form>
      {/if}
      <div class="field"><span>When to use it</span>
        <div class="modes">
          {#each modes as [key, label, desc]}
            <label class="toggle">
              <input type="radio" name="ntfymode" checked={nt.mode === key} onchange={() => setMode(key)} />
              <span><strong>{label}</strong><small>{desc}</small></span>
            </label>
          {/each}
        </div>
      </div>
      <p class="hint">Server scripts (ZFS, SMART, the mover…) use this too. If neither Discord nor ntfy works, the message is still written to the server's log: <code>journalctl -t notify-discord</code>.</p>
    </div>
  </section>

  <section class="card" class:off={!info.configured && !info.ntfy.configured}>
    <div class="card-header"><h2>What to send</h2>{#if !info.configured && !info.ntfy.configured}<span class="dim small">Connect Discord or ntfy first</span>{/if}</div>
    <div class="card-body groups">
      {#each groups as g}
        <div class="group">
          <h3>{g.name}</h3>
          {#each g.events as e}
            <label class="toggle">
              <input type="checkbox" checked={e.enabled} disabled={!info.configured && !info.ntfy.configured} onchange={(ev) => put({ events: { [e.key]: ev.target.checked } })} />
              <span><strong>{e.label}</strong><small>{e.description}</small></span>
            </label>
          {/each}
        </div>
      {/each}
      <p class="hint">Repeats are grouped: a container that keeps crashing sends one "keeps restarting" message, not one per crash. Nobody is ever @mentioned.</p>
    </div>
  </section>

  <section class="card">
    <div class="card-header"><h2>Alerts from the server's own scripts</h2></div>
    <div class="card-body stack-v">
      <p class="small">ZFS, SMART, disk space checks and the mover can post to the same channel with the <code>homestack notify</code> command. They use the Discord and ntfy settings saved here, and send nothing while both are removed or "Server alerts" is off.</p>
      <pre class="fix">docker exec homestack homestack notify -level warning -title "Disk space" "The cache is 92% full"</pre>
      <p class="hint">Ready-made hooks for ZFS, SMART and disk space are in <code>deploy/debian/notify/</code>.</p>
    </div>
  </section>

  {#if info.recent.length}
    <section class="card">
      <div class="card-header"><h2>Recently sent</h2><span class="dim small">since Homestack started</span></div>
      <div class="recent">
        {#each info.recent as r}
          <div class="sent {r.ok ? r.level : 'failed'}">
            <Icon name={r.ok ? levelIcon[r.level] || 'info' : 'x'} size={15} />
            <span class="grow"><span class="title">{r.title}</span>{#if r.ok && r.error}<small class="warntext">{r.error}</small>{:else if r.error}<small class="errtext">{r.error}</small>{/if}</span>
            {#if r.via}<span class="via">{r.via.replace('+', ' + ')}</span>{/if}
            <span class="dim small">{ago(r.time)}</span>
          </div>
        {/each}
      </div>
    </section>
  {/if}
{/if}

<style>
  .row { display: flex; gap: 8px; align-items: center; }
  .wrap { flex-wrap: wrap; }
  .grow { flex: 1; min-width: 0; overflow-wrap: anywhere; }
  .small { font-size: 13px; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .steps { margin: 0; padding-left: 20px; display: grid; gap: 4px; font-size: 13.5px; color: var(--text-2); }
  .pill { display: inline-flex; align-items: center; gap: 4px; padding: 2px 8px; border-radius: 99px; font-size: 12px; font-weight: 500; }
  .pill.ok { background: var(--green-soft); color: var(--green-text); }
  .off .groups { opacity: 0.55; }
  .groups { display: grid; gap: 18px; }
  .group { display: grid; gap: 10px; }
  .group h3 { margin: 0; font-size: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: var(--text-3); }
  .toggle { display: flex; gap: 10px; align-items: flex-start; cursor: pointer; }
  .toggle input { margin-top: 3px; width: 16px; height: 16px; accent-color: var(--accent); flex: none; }
  .toggle span { display: grid; }
  .toggle small { color: var(--text-3); font-size: 12.5px; }
  .fix { margin: 0; padding: 10px 12px; background: var(--surface-2); border: 1px solid var(--border); border-radius: 8px; white-space: pre-wrap; overflow-wrap: anywhere; font-size: 12px; }
  .recent { display: grid; }
  .sent { display: flex; gap: 10px; align-items: flex-start; padding: 10px 18px; border-bottom: 1px solid var(--border); font-size: 13.5px; }
  .sent:last-child { border-bottom: 0; }
  .sent > :global(svg) { flex: none; margin-top: 2px; }
  .sent.success > :global(svg) { color: var(--green); }
  .sent.info > :global(svg) { color: var(--accent); }
  .sent.warning > :global(svg) { color: var(--amber); }
  .sent.error > :global(svg), .sent.failed > :global(svg) { color: var(--red); }
  .sent .grow { display: grid; }
  .errtext { color: var(--red-text); font-size: 12.5px; }
  .warntext { color: var(--amber-text); font-size: 12.5px; }
  .via { font-size: 11.5px; color: var(--text-3); border: 1px solid var(--border); border-radius: 99px; padding: 0 7px; white-space: nowrap; }
  .ntgrid { display: grid; grid-template-columns: minmax(0, 1.4fr) minmax(0, 1fr) minmax(0, 1fr); gap: 12px; }
  .modes { display: grid; gap: 8px; }
  .dim-2 { color: var(--text-2); margin: 0; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  @media (max-width: 720px) { .ntgrid { grid-template-columns: 1fr; } }
  .err { display: flex; gap: 8px; align-items: center; color: var(--red-text); }
</style>
