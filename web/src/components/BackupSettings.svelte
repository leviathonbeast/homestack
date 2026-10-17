<script>
  // Settings → Backups: the passphrase, where backups go, the nightly
  // schedule, the saved ones (download or restore) and restoring a file.
  // The host agent makes them; see internal/agent/backup.go.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast } from '../lib/state.svelte.js';
  import { bytes, dateTime, ago } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from './Modal.svelte';
  import RestoreBackup from './RestoreBackup.svelte';

  let v = $state(null);
  let agentError = $state('');
  let sched = $state(null);
  let pass1 = $state('');
  let pass2 = $state('');
  let changing = $state(false);
  let dir = $state('');
  let keep = $state(14);
  let busy = $state('');
  let restoring = $state(null); // { name } of a saved one, or {} for a file

  async function load() {
    try {
      v = await api.get('/backups');
      dir = v.dir;
      keep = v.keep;
      agentError = '';
    } catch (e) {
      agentError = e.message;
    }
    try {
      sched = (await api.get('/host/schedules')).tasks.find((t) => t.id === 'backup') || null;
    } catch {
      sched = null;
    }
  }
  onMount(load);

  async function run(what, fn) {
    busy = what;
    try {
      await fn();
    } catch (e) {
      toast(e.message, 'error', 10000);
    }
    busy = '';
  }
  const setPass = () => run('pass', async () => {
    if (pass1 !== pass2) throw new Error("The two passphrases don't match.");
    v = await api.put('/backups/settings', { passphrase: pass1 });
    pass1 = pass2 = '';
    changing = false;
    toast('Backup passphrase saved. Keep it somewhere other than this server: a restore needs it.', 'success', 8000);
    load();
  });
  const saveFolder = () => run('folder', async () => {
    v = await api.put('/backups/settings', { dir: dir.trim(), keep: +keep });
    toast('Saved', 'success', 2500);
  });
  const backupNow = (download) => run(download ? 'download' : 'now', async () => {
    const r = await api.post('/backups');
    v = r.view;
    toast(`Saved ${r.file.name}`, 'success', 4000);
    if (download) location.href = fileUrl(r.file.name);
  });
  const nightly = (on) => run('nightly', async () => {
    await api.put('/host/schedules', { schedules: [{ ...sched.schedule, id: 'backup', enabled: on }] });
    await load();
  });
  const fileUrl = (n) => `/api/backups/file?name=${encodeURIComponent(n)}`;
  const at = (s) => `${String(s.hour).padStart(2, '0')}:${String(s.minute).padStart(2, '0')}`;
</script>

{#if agentError}
  <section class="card">
    <div class="card-header"><h2>Backups</h2></div>
    <div class="card-body stack-v">
      <div class="note"><Icon name="info" size={15} /><span>Backups are made by the host agent, which isn't reachable ({agentError}). Install it with <code>deploy/debian/agent/install.sh</code>. You can still restore Homestack's own settings from a file below.</span></div>
    </div>
  </section>
{:else if !v}
  <div class="skeleton" style="height:240px"></div>
{:else}
  <section class="card">
    <div class="card-header"><h2>Backups</h2><span class="dim small hide-sm">For when the boot drive dies</span></div>
    <div class="card-body stack-v">
      <p class="intro">One encrypted file with Homestack's settings and everything it set up on the server: the array's mounts and snapraid.conf, the shares and their users, schedules and alerts, fan curves, the UPS, VM definitions and the network. Not your files or VM disks: they're on the array. See <code>RECOVERY.md</code> for bringing a server back from one.</p>
      {#if v.dirProblem}<div class="note amber"><Icon name="alert" size={15} /><span>{v.dirProblem}</span></div>{/if}
      <dl class="kv">
        <dt>Passphrase</dt>
        <dd>{#if v.ready}<span class="ok"><Icon name="check" size={14} /> Set</span>{:else}<span class="warn">Not set: nothing can be backed up yet</span>{/if}</dd>
        <dt>Every night</dt>
        <dd>
          {#if !sched}<span class="dim">–</span>
          {:else if sched.schedule.enabled}<span class="ok"><Icon name="check" size={14} /> At {at(sched.schedule)}</span>{#if sched.next}<span class="dim"> · next {ago(sched.next)}</span>{/if}
            <button class="sm ghost" onclick={() => nightly(false)} disabled={!!busy}>Turn off</button>
          {:else}<span class="warn">Off</span>
            <button class="sm" onclick={() => nightly(true)} disabled={!!busy || !!sched.blocked} title={sched.blocked || ''}>Turn on, at {at(sched.schedule)}</button>
            {#if sched.blocked}<span class="dim small">{sched.blocked}</span>{/if}
          {/if}
          <a class="small" href="/schedules">Change the time</a>
        </dd>
        <dt>Last backup</dt>
        <dd>{#if v.last}<span class={v.last.ok ? '' : 'bad'}>{v.last.summary}</span> <span class="dim">{ago(v.last.finished)}</span>{:else}<span class="dim">None yet</span>{/if}</dd>
      </dl>
      <div class="row">
        <button class="primary" onclick={() => backupNow(false)} disabled={!!busy || !v.ready || !!v.dirProblem}>{#if busy === 'now'}<Icon name="loader" size={14} class="spin" />{:else}<Icon name="archive" size={14} />{/if} Back up now</button>
        <button onclick={() => backupNow(true)} disabled={!!busy || !v.ready || !!v.dirProblem}>{#if busy === 'download'}<Icon name="loader" size={14} class="spin" />{:else}<Icon name="download" size={14} />{/if} Back up and download</button>
      </div>
    </div>
  </section>

  <section class="card">
    <div class="card-header"><h2>Passphrase</h2></div>
    <div class="card-body stack-v">
      {#if v.ready && !changing}
        <p class="intro">Backups are encrypted with it, and restoring one needs it. Homestack keeps only a key made from it, so it can't show it to you: keep it in your password manager.</p>
        <div class="row"><button onclick={() => (changing = true)}>Change the passphrase</button></div>
      {:else}
        <p class="intro">{v.ready ? 'New backups use the new passphrase; the ones already saved still need the old one.' : 'Pick something long you won\'t forget, like a few unrelated words. Keep it somewhere other than this server, like your password manager: restoring after a boot-drive failure needs it.'}</p>
        <form class="stack-v narrow" onsubmit={(e) => { e.preventDefault(); setPass(); }}>
          <label class="field"><span>Passphrase (12 characters or more)</span><input type="password" bind:value={pass1} minlength="12" autocomplete="new-password" required /></label>
          <label class="field"><span>Again</span><input type="password" bind:value={pass2} autocomplete="new-password" required /></label>
          <div class="row">
            <button class="primary" type="submit" disabled={busy === 'pass' || pass1.length < 12 || pass1 !== pass2}>Save passphrase</button>
            {#if changing}<button type="button" class="ghost" onclick={() => (changing = false)}>Cancel</button>{/if}
          </div>
        </form>
      {/if}
    </div>
  </section>

  <section class="card">
    <div class="card-header"><h2>Where they're kept</h2></div>
    <form class="card-body stack-v" onsubmit={(e) => { e.preventDefault(); saveFolder(); }}>
      <div class="row wrap">
        <label class="field grow"><span>Folder</span><input class="mono" bind:value={dir} spellcheck="false" autocapitalize="off" /></label>
        <label class="field"><span>Keep the newest</span><input type="number" min="1" max="365" bind:value={keep} style="width:100px" /></label>
      </div>
      <span class="hint">Anywhere off the boot drive, which is what they're for. On the array they're covered by parity too; also download one now and then and keep it elsewhere.</span>
      <div class="row"><button type="submit" disabled={busy === 'folder' || (dir === v.dir && +keep === v.keep)}>Save</button></div>
    </form>
  </section>

  <section class="card">
    <div class="card-header"><h2>Saved backups</h2><span class="dim small">{v.files.length} in {v.dir}</span></div>
    {#if v.files.length}
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>Made</th><th class="hide-sm">File</th><th>Size</th><th></th></tr></thead>
          <tbody>
            {#each v.files as f (f.name)}
              <tr>
                <td class="nowrap">{dateTime(f.created)}{#if f.dir && f.dir !== v.dir}<div class="dim small">on {f.on || f.dir}</div>{/if}</td>
                <td class="hide-sm mono small">{f.name}</td>
                <td class="nowrap">{bytes(f.size)}</td>
                <td class="actions">
                  <a class="btn sm" href={fileUrl(f.name)} download><Icon name="download" size={13} /> Download</a>
                  <button class="sm ghost" onclick={() => (restoring = { name: f.name })}>Restore</button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {:else}
      <div class="card-body dim small">None yet.</div>
    {/if}
  </section>
{/if}

<section class="card">
  <div class="card-header"><h2>Restore from a file</h2></div>
  <div class="card-body stack-v">
    <p class="intro">A backup you downloaded, or copied off the array. You pick which parts come back.</p>
    <div class="row"><button onclick={() => (restoring = {})}><Icon name="upgrade" size={14} /> Restore a backup file…</button></div>
  </div>
</section>

{#if restoring}
  <Modal title={restoring.name ? `Restore ${restoring.name}` : 'Restore a backup file'} width={620} onclose={() => (restoring = null)}>
    <RestoreBackup name={restoring.name || ''} onrestored={(out) => { if (!out) load(); }} />
  </Modal>
{/if}

<style>
  .intro { margin: 0; font-size: 13.5px; line-height: 1.55; color: var(--text-2); }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; border-radius: 10px; font-size: 13px; background: var(--surface-2); color: var(--text-2); }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .kv { display: grid; grid-template-columns: max-content 1fr; gap: 10px 18px; margin: 0; font-size: 13.5px; align-items: center; }
  .kv dt { color: var(--text-2); }
  .kv dd { margin: 0; display: flex; gap: 8px; align-items: center; flex-wrap: wrap; min-width: 0; }
  .ok { display: inline-flex; gap: 5px; align-items: center; color: var(--green-text); }
  .warn { color: var(--amber-text); }
  .bad { color: var(--red-text); }
  .row { display: flex; gap: 8px; align-items: flex-end; flex-wrap: wrap; }
  .grow { flex: 1; min-width: 220px; }
  .field { display: grid; gap: 6px; font-size: 13px; }
  .narrow { max-width: 420px; }
  .small { font-size: 12.5px; }
</style>
