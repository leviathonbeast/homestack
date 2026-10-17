<script>
  // Restoring a backup: open it with its passphrase, pick the parts, see how
  // each went. In Settings → Backups and on the first-run screen (before
  // there's an account, only Homestack's own settings unless the host agent
  // is already installed).
  import { api } from '../lib/api.js';
  import { dateTime } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';

  let { name = '', onrestored = () => {} } = $props();

  let file = $state(null);
  let pass = $state('');
  let opened = $state(null);
  let picked = $state({});
  let typed = $state('');
  let busy = $state(false);
  let error = $state('');
  let results = $state(null);
  let signedOut = $state(false);

  async function toBase64(f) {
    const buf = new Uint8Array(await f.arrayBuffer());
    let s = '';
    for (let i = 0; i < buf.length; i += 0x8000) s += String.fromCharCode(...buf.subarray(i, i + 0x8000));
    return btoa(s);
  }

  async function open(e) {
    e?.preventDefault();
    busy = true;
    error = '';
    try {
      const body = { passphrase: pass };
      if (name) body.name = name;
      else body.file = await toBase64(file);
      opened = await api.post('/backups/open', body);
      picked = Object.fromEntries(opened.parts.map((p) => [p.id, p.default && p.available]));
    } catch (err) {
      error = err.message;
    }
    busy = false;
  }

  const chosen = $derived(opened ? opened.parts.filter((p) => picked[p.id]).map((p) => p.id) : []);
  const unavailable = $derived(opened ? opened.parts.filter((p) => !p.available) : []);

  async function restore() {
    busy = true;
    error = '';
    try {
      const r = await api.post('/backups/restore', { token: opened.token, parts: chosen });
      results = r.results;
      signedOut = r.signedOut;
      onrestored(r.signedOut);
    } catch (err) {
      error = err.message;
    }
    busy = false;
  }
  const labelOf = (id) => (id === 'passphrase' ? 'Backup passphrase' : opened?.parts.find((p) => p.id === id)?.label || id);
</script>

{#if results}
  <div class="stack-v">
    <ul class="results">
      {#each results as r}
        <li class={r.ok ? 'ok' : 'bad'}>
          <Icon name={r.ok ? 'check' : 'alert'} size={15} />
          <div><strong>{labelOf(r.part)}</strong><div class="small">{r.detail}</div>
            {#if r.warn}<div class="small warn"><Icon name="alert" size={12} /> {r.warn}</div>{/if}
            {#if r.saved?.length}<div class="dim small">Kept the files it replaced: {r.saved.join(', ')}</div>{/if}</div>
        </li>
      {/each}
    </ul>
    {#if signedOut}
      <div class="note"><Icon name="info" size={15} /><span>Homestack's settings are back, so everyone is signed out. Sign in with the restored account{unavailable.length ? '; to restore the server\'s parts, install the host agent, then restore this backup again from Settings → Backups' : ''}.</span></div>
      <button class="primary" onclick={() => location.reload()}>Sign in</button>
    {/if}
  </div>
{:else if opened}
  <div class="stack-v">
    <p class="small dim">From <strong>{opened.host}</strong>, {dateTime(opened.created)}{opened.version ? `, Homestack ${opened.version}` : ''}.</p>
    {#if unavailable.length}
      <div class="note"><Icon name="info" size={15} /><span>The server's parts need the host agent, which isn't running yet. Restore Homestack's settings now, sign in, install the agent (<code>sudo bash …/deploy/debian/agent/install.sh</code>), then restore this backup again from Settings → Backups for the rest.</span></div>
    {/if}
    {#if opened.missing?.length}
      <div class="note amber"><Icon name="alert" size={15} /><span>When it was made, these couldn't be read, so they're not in it: {opened.missing.join('; ')}.</span></div>
    {/if}
    <div class="parts">
      {#each opened.parts as p (p.id)}
        <label class="part" class:off={!p.available} class:risky={p.id === 'network' && picked[p.id]}>
          <input type="checkbox" bind:checked={picked[p.id]} disabled={!p.available || busy} />
          <span>
            <span><strong>{p.label}</strong>{#if p.note}<span class="dim small"> · {p.note}</span>{/if}</span>
            <span class="small dim">{p.desc}</span>
            {#if p.why}<span class="small why">{p.why}</span>{/if}
          </span>
        </label>
      {/each}
    </div>
    <p class="small dim">Each file it replaces is kept next to it (<code>….homestack-restore-…</code>). The array's mounts are added to <code>/etc/fstab</code>, never this install's own.</p>
    <label class="field">
      <span>Type RESTORE to confirm</span>
      <input bind:value={typed} autocapitalize="characters" spellcheck="false" autocomplete="off" />
    </label>
    {#if error}<p class="error-text">{error}</p>{/if}
    <div class="row">
      <button class="primary" onclick={restore} disabled={busy || !chosen.length || typed.trim() !== 'RESTORE'}>
        {#if busy}<Icon name="loader" size={14} class="spin" />{/if} Restore {chosen.length} part{chosen.length === 1 ? '' : 's'}
      </button>
    </div>
  </div>
{:else}
  <form class="stack-v" onsubmit={open}>
    {#if !name}
      <label class="field">
        <span>The backup file (.hsbak)</span>
        <input type="file" accept=".hsbak" onchange={(e) => (file = e.target.files[0] || null)} />
      </label>
    {/if}
    <label class="field">
      <span>Its passphrase</span>
      <input type="password" bind:value={pass} autocomplete="off" />
    </label>
    {#if error}<p class="error-text">{error}</p>{/if}
    <div class="row">
      <button class="primary" type="submit" disabled={busy || !pass || (!name && !file)}>
        {#if busy}<Icon name="loader" size={14} class="spin" />{/if} Open
      </button>
    </div>
  </form>
{/if}

<style>
  .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .small { font-size: 12.5px; }
  .field { display: grid; gap: 6px; font-size: 13px; }
  p { margin: 0; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; border-radius: 10px; font-size: 13px; background: var(--surface-2); color: var(--text-2); }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .parts { display: grid; gap: 2px; border: 1px solid var(--border); border-radius: 10px; padding: 4px; }
  .part { display: flex; gap: 10px; align-items: flex-start; padding: 9px 10px; border-radius: 8px; cursor: pointer; }
  .part:hover { background: var(--surface-2); }
  .part input { margin-top: 3px; width: 16px; height: 16px; accent-color: var(--accent); flex: none; }
  .part > span { display: grid; gap: 2px; font-size: 13.5px; }
  .part.off { opacity: 0.75; cursor: default; }
  .part.risky { background: var(--amber-soft); }
  .why { color: var(--amber-text); }
  .results { list-style: none; margin: 0; padding: 0; display: grid; gap: 8px; }
  .results li { display: flex; gap: 10px; align-items: flex-start; font-size: 13.5px; }
  .results li > :global(svg) { flex: none; margin-top: 2px; }
  .results li.ok > :global(svg) { color: var(--green); }
  .results li.bad > :global(svg) { color: var(--red); }
  .results .warn { color: var(--amber-text); margin-top: 3px; }
</style>
