<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { toast, confirm } from '../lib/state.svelte.js';
  import { ago } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Modal from '../components/Modal.svelte';

  let v = $state(null);
  let error = $state('');
  let tab = $state('shares');
  let busy = $state(false);
  let edit = $state(null); // { original, share, createFolder }
  let userForm = $state(null); // { mode: 'add'|'password', user, password, password2 }

  async function load() {
    try {
      v = await api.get('/host/shares');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(() => {
    load();
    const t = setInterval(() => { if (!edit && !userForm) load(); }, 15000);
    return () => clearInterval(t);
  });

  async function op(body, msg) {
    busy = true;
    try {
      v = await api.post('/host/shares', body);
      if (msg) toast(msg, 'success', 2500);
      busy = false;
      return true;
    } catch (e) {
      toast(e.message, 'error', 9000);
      busy = false;
      return false;
    }
  }

  const secLabel = { public: 'Public', secure: 'Secure', private: 'Private' };
  const secHelp = {
    public: 'Anyone on the network can read and write, no password.',
    secure: 'Anyone can read without a password; only the users you pick can write.',
    private: 'Only the users you pick can connect, each read-only or read/write.',
  };
  const expLabel = { yes: 'Shown', hidden: 'Hidden', no: 'Off' };
  // The shares' folder (Settings → Storage), like /mnt/user.
  const root = $derived(v?.root || '/mnt/user');
  // The mover only handles top-level folders in the shares without spaces or quotes.
  const moverOK = (p) => {
    const rest = (p || '').startsWith(root + '/') ? p.slice(root.length + 1).replace(/\/$/, '') : '';
    return !!rest && !/[/\s"'$`\\]/.test(rest);
  };

  function who(s) {
    if (s.security === 'public') return 'Everyone: read/write';
    const e = Object.entries(s.access || {});
    if (s.security === 'secure') {
      const rw = e.filter(([, a]) => a === 'rw').map(([u]) => u);
      return `Everyone reads${rw.length ? ` · ${rw.join(', ')} write` : ''}`;
    }
    if (!e.length) return 'Nobody yet';
    return e.map(([u, a]) => `${u} ${a === 'rw' ? '(rw)' : '(ro)'}`).join(', ');
  }

  function newShare() {
    edit = { original: '', createFolder: true, share: { name: '', path: root + '/', comment: '', export: 'yes', security: 'private', access: {}, mover: false } };
  }
  function editShare(s) {
    edit = { original: s.name, createFolder: false, share: JSON.parse(JSON.stringify(s)) };
  }
  function setAccess(u, val) {
    const a = { ...edit.share.access };
    if (val) a[u] = val; else delete a[u];
    edit.share.access = a;
  }
  function onName() {
    if (!edit.original && (edit.share.path === root + '/' || edit.share.path.startsWith(root + '/') && !edit.pathTouched)) {
      edit.share.path = root + '/' + edit.share.name;
    }
  }
  async function saveShare(e) {
    e.preventDefault();
    if (!moverOK(edit.share.path)) edit.share.mover = false;
    if (await op({ op: 'save', original: edit.original, share: edit.share, createFolder: edit.createFolder }, 'Share saved')) edit = null;
  }
  async function deleteShare(s) {
    const ok = await confirm({
      title: `Stop sharing ${s.name}?`,
      message: `The share disappears from the network. The folder ${s.path} and everything in it stay exactly where they are.`,
      confirmLabel: 'Stop sharing', danger: true,
    });
    if (ok) await op({ op: 'delete', original: s.name }, 'Share removed');
  }
  async function takeover() {
    const ok = await confirm({
      title: 'Manage these shares here?',
      message: `Homestack moves the share sections out of ${v.conf} into its own file (a backup of smb.conf is kept next to it), checks the result with testparm, and reloads Samba. Settings in [global] stay where they are.`,
      confirmLabel: 'Take over',
    });
    if (ok) await op({ op: 'takeover' }, 'Shares imported');
  }

  async function saveUser(e) {
    e.preventDefault();
    if (userForm.password !== userForm.password2) return toast("The passwords don't match.", 'error');
    const body = { op: userForm.mode === 'add' ? 'user-add' : 'user-password', user: userForm.user, password: userForm.password };
    if (await op(body, userForm.mode === 'add' ? 'User added' : 'Password changed')) userForm = null;
  }
  async function removeUser(u) {
    const ok = await confirm({
      title: `Remove ${u.name}?`,
      message: `${u.name} can no longer connect to any share, and is removed from every share's access list.${u.createdHere ? ' The Linux account Homestack made for them is removed too.' : ' Their Linux account is left as it is.'} No files are deleted.`,
      confirmLabel: 'Remove', danger: true,
    });
    if (ok) await op({ op: 'user-remove', user: u.name }, 'User removed');
  }
</script>

<div class="page">
  <div class="page-header">
    <div>
      <h1>Shares</h1>
      <p class="sub">Network shares (SMB) and who can use them.</p>
    </div>
    {#if v?.managed && tab === 'shares'}<button class="primary" onclick={newShare}><Icon name="plus" size={15} /> Add share</button>{/if}
    {#if v?.samba && tab === 'users'}<button class="primary" onclick={() => (userForm = { mode: 'add', user: '', password: '', password2: '' })}><Icon name="plus" size={15} /> Add user</button>{/if}
  </div>

  {#if error}
    <div class="card note err"><Icon name="alert" size={16} /> {error}</div>
  {:else if !v}
    <div class="skeleton" style="height:300px"></div>
  {:else if !v.samba}
    <div class="note warning"><Icon name="alert" size={16} /> {v.error}</div>
  {:else}
    {#if v.error}<div class="note warning"><Icon name="alert" size={16} /> {v.error}</div>{/if}
    <div class="segmented sharetabs">
      <button class:active={tab === 'shares'} onclick={() => (tab = 'shares')}>Shares</button>
      <button class:active={tab === 'users'} onclick={() => (tab = 'users')}>Users <span class="n">{v.users.length}</span></button>
      <button class:active={tab === 'connected'} onclick={() => (tab = 'connected')}>Connected <span class="n">{v.sessions.length}</span></button>
    </div>

    {#if tab === 'shares'}
      {#if !v.managed}
        <div class="card takeover">
          <div class="grow">
            <strong>These shares are set up by hand in {v.conf}.</strong>
            <p class="small dim">Take them over to manage them here. Their current settings are carried across as shown below, and smb.conf is backed up first.</p>
          </div>
          <button class="primary" onclick={takeover} disabled={busy}>Take over</button>
        </div>
      {/if}
      {#if v.missingFolders?.length}
        <div class="note warning"><Icon name="alert" size={16} /> Folder missing for: {v.missingFolders.join(', ')}. Those shares won't open.</div>
      {/if}
      {@const list = v.managed ? v.shares : v.existing || []}
      <section class="card">
        {#if list.length}
          <div class="table-wrap">
            <table class="table">
              <thead><tr><th>Share</th><th class="hide-sm">Folder</th><th>Security</th><th class="hide-sm">Access</th><th></th></tr></thead>
              <tbody>
                {#each list as s (s.name)}
                  <tr class:off={s.export === 'no'}>
                    <td><strong>{s.name}</strong>{#if s.export !== 'yes'}<span class="badge">{expLabel[s.export]}</span>{/if}{#if s.mover}<span class="badge mover" title="Moved from the cache to the array nightly">Mover</span>{/if}{#if s.comment}<div class="dim small">{s.comment}</div>{/if}</td>
                    <td class="hide-sm mono small">{s.path}</td>
                    <td><span class="sec {s.security}">{secLabel[s.security]}</span></td>
                    <td class="hide-sm small">{who(s)}</td>
                    <td class="actions">
                      {#if v.managed}
                        <button class="sm" onclick={() => editShare(s)}>Edit</button>
                        <button class="sm ghost icon" aria-label="Stop sharing {s.name}" onclick={() => deleteShare(s)}><Icon name="trash" size={14} /></button>
                      {/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {:else}
          <div class="card-body dim">No shares yet.</div>
        {/if}
      </section>
      <p class="dim small tail">Connect from Windows with <code>\\{v.host || 'server'}\ShareName</code>, from a Mac with <code>smb://{v.host || 'server'}/ShareName</code>. Removing a share never deletes its files.</p>
    {:else if tab === 'users'}
      <section class="card">
        {#if v.users.length}
          <div class="table-wrap">
            <table class="table">
              <thead><tr><th>User</th><th>Can use</th><th></th></tr></thead>
              <tbody>
                {#each v.users as u (u.name)}
                  {@const mine = (v.managed ? v.shares : v.existing || []).filter((s) => s.security === 'public' || s.access?.[u.name] || s.security === 'secure')}
                  <tr>
                    <td><strong>{u.name}</strong><div class="dim small">UID {u.uid}</div></td>
                    <td class="small">{mine.map((s) => s.name).join(', ') || '–'}</td>
                    <td class="actions">
                      <button class="sm" onclick={() => (userForm = { mode: 'password', user: u.name, password: '', password2: '' })}>Password</button>
                      <button class="sm ghost icon" aria-label="Remove {u.name}" onclick={() => removeUser(u)}><Icon name="trash" size={14} /></button>
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {:else}
          <div class="card-body dim">No Samba users yet. Add one to use Secure or Private shares.</div>
        {/if}
      </section>
      <p class="dim small tail">Samba passwords are separate from Linux login passwords. People sign in to shares with these.</p>
    {:else}
      <section class="card">
        {#if v.sessions.length}
          <div class="table-wrap">
            <table class="table">
              <thead><tr><th>User</th><th>Computer</th><th>Using</th><th class="hide-sm">Protocol</th></tr></thead>
              <tbody>
                {#each v.sessions as c}
                  <tr><td>{c.user || 'guest'}</td><td class="mono small">{c.machine}</td><td class="small">{c.shares.join(', ') || '–'}</td><td class="hide-sm small dim">{c.protocol}</td></tr>
                {/each}
              </tbody>
            </table>
          </div>
        {:else}
          <div class="card-body dim">Nobody is connected{v.checked ? ` (checked ${ago(v.checked)})` : ''}.</div>
        {/if}
      </section>
    {/if}
  {/if}
</div>

{#if edit}
  <Modal title={edit.original ? `Edit ${edit.original}` : 'Add a share'} width={620} onclose={() => (edit = null)}>
    <form class="form" onsubmit={saveShare} id="shareform">
      <label class="field"><span>Name</span>
        <input bind:value={edit.share.name} oninput={onName} required maxlength="64" placeholder="Photos" /></label>
      <label class="field"><span>Folder</span>
        <input class="mono" bind:value={edit.share.path} oninput={() => (edit.pathTouched = true)} required />
        <span class="hint">{root}/Name spreads the share across the cache and all disks, like Unraid. A path on one disk, or an unassigned disk, also works.</span></label>
      {#if !edit.original}
        <label class="check"><input type="checkbox" bind:checked={edit.createFolder} /> Create the folder if it doesn't exist (owner {v?.owner || 'nobody:users'})</label>
      {/if}
      <label class="field"><span>Description <small class="dim">(optional)</small></span><input bind:value={edit.share.comment} maxlength="120" /></label>
      <label class="check" class:disabled={!moverOK(edit.share.path)}>
        <input type="checkbox" bind:checked={edit.share.mover} disabled={!moverOK(edit.share.path)} />
        <span><strong>Move to the array nightly</strong><br /><small class="dim">{moverOK(edit.share.path)
          ? 'New files land on the fast cache; the mover moves them onto the array (and into parity) each night. Leave off to keep this share on the cache.'
          : `Only for shares in ${root} whose folder name has no spaces.`}</small></span>
      </label>
      <div class="two">
        <label class="field"><span>Export</span>
          <select bind:value={edit.share.export}>
            <option value="yes">Yes: shown on the network</option>
            <option value="hidden">Hidden: works if you type its name</option>
            <option value="no">No: switched off</option>
          </select></label>
        <label class="field"><span>Security</span>
          <select bind:value={edit.share.security}>
            <option value="public">Public</option>
            <option value="secure">Secure</option>
            <option value="private">Private</option>
          </select></label>
      </div>
      <p class="hint">{secHelp[edit.share.security]}</p>
      {#if edit.share.security !== 'public'}
        <div class="field"><span>User access</span>
          {#if v.users.length}
            <div class="grid">
              {#each v.users as u (u.name)}
                {@const cur = edit.share.access[u.name] || ''}
                <div class="urow">
                  <strong>{u.name}</strong>
                  <div class="segmented sm">
                    {#if edit.share.security === 'private'}<button type="button" class:active={cur === ''} onclick={() => setAccess(u.name, '')}>No access</button>{/if}
                    <button type="button" class:active={edit.share.security === 'private' ? cur === 'ro' : cur !== 'rw'} onclick={() => setAccess(u.name, edit.share.security === 'private' ? 'ro' : '')}>Read-only</button>
                    <button type="button" class:active={cur === 'rw'} onclick={() => setAccess(u.name, 'rw')}>Read/write</button>
                  </div>
                </div>
              {/each}
            </div>
          {:else}
            <p class="hint">No Samba users yet. Add one on the Users tab first.</p>
          {/if}
        </div>
      {/if}
    </form>
    {#snippet footer()}
      <button onclick={() => (edit = null)}>Cancel</button>
      <button class="primary" type="submit" form="shareform" disabled={busy}>{edit.original ? 'Save' : 'Add share'}</button>
    {/snippet}
  </Modal>
{/if}

{#if userForm}
  <Modal title={userForm.mode === 'add' ? 'Add a user' : `New password for ${userForm.user}`} onclose={() => (userForm = null)}>
    <form class="form" onsubmit={saveUser} id="userform">
      {#if userForm.mode === 'add'}
        <label class="field"><span>User name</span><input bind:value={userForm.user} required maxlength="32" pattern="[a-z_][a-z0-9_-]*" autocomplete="off" placeholder="dune" />
          <span class="hint">Lower-case letters, numbers, - and _.</span></label>
      {/if}
      <label class="field"><span>Password</span><input type="password" bind:value={userForm.password} required minlength="8" autocomplete="new-password" /></label>
      <label class="field"><span>Password again</span><input type="password" bind:value={userForm.password2} required minlength="8" autocomplete="new-password" /></label>
    </form>
    {#snippet footer()}
      <button onclick={() => (userForm = null)}>Cancel</button>
      <button class="primary" type="submit" form="userform" disabled={busy}>{userForm.mode === 'add' ? 'Add user' : 'Change password'}</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 11px 14px; border-radius: 10px; margin-bottom: 12px; font-size: 13.5px; }
  .note.warning { background: var(--amber-soft); color: var(--amber-text); }
  .note.err { background: var(--red-soft); color: var(--red-text); }
  .note :global(svg) { flex: none; margin-top: 1px; }
  .sharetabs { margin-bottom: 14px; }
  .n { margin-left: 4px; font-size: 11px; opacity: 0.7; }
  .takeover { display: flex; gap: 14px; align-items: center; padding: 14px 18px; margin-bottom: 14px; border-color: color-mix(in srgb, var(--accent) 45%, var(--border)); }
  .takeover p { margin: 4px 0 0; }
  .grow { flex: 1; min-width: 0; }
  .small { font-size: 12.5px; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  .actions { text-align: right; white-space: nowrap; }
  .badge.mover { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent); border-color: transparent; }
  .check.disabled { opacity: 0.55; }
  .badge { margin-left: 8px; font-size: 11px; padding: 1px 7px; border-radius: 99px; background: var(--surface-2); border: 1px solid var(--border); color: var(--text-2); }
  .sec { font-size: 12px; font-weight: 600; padding: 2px 8px; border-radius: 99px; }
  .sec.public { background: var(--amber-soft); color: var(--amber-text); }
  .sec.secure { background: color-mix(in srgb, var(--accent) 18%, transparent); color: var(--accent); }
  .sec.private { background: var(--green-soft); color: var(--green-text); }
  tr.off td { opacity: 0.55; }
  .tail { margin-top: 14px; }
  .form { display: grid; gap: 14px; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
  .check { display: flex; gap: 8px; align-items: flex-start; font-size: 13px; }
  .check input { margin-top: 3px; }
  .grid { display: grid; gap: 6px; max-height: 260px; overflow: auto; }
  .urow { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 4px 0; border-bottom: 1px solid var(--border); }
  .urow:last-child { border-bottom: 0; }
  .segmented.sm button { height: 26px; font-size: 12px; padding: 0 9px; }
  @media (max-width: 560px) { .two { grid-template-columns: 1fr; } .urow { flex-direction: column; align-items: flex-start; } }
</style>
