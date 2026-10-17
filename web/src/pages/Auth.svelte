<script>
  // First-run setup (system check, then admin account) and the sign-in screen.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { session, toast } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import { copy as copyText } from '../lib/clipboard.js';
  import RestoreBackup from '../components/RestoreBackup.svelte';
  import { sourceUrl } from '../lib/format.js';

  const setup = $derived(session.setupRequired);
  let step = $state('check'); // check | account | restore

  // ---- System check ----
  let checks = $state([]);
  let checking = $state(false);
  let checkError = $state('');
  const failed = $derived(checks.filter((c) => c.status === 'fail').length);
  const warned = $derived(checks.filter((c) => c.status === 'warn').length);

  async function runChecks() {
    checking = true;
    checkError = '';
    try {
      const res = await api.get('/preflight');
      checks = res.checks;
    } catch (e) {
      checkError = e.message;
    }
    checking = false;
  }

  onMount(() => {
    if (setup) runChecks();
  });

  const copy = (text) => copyText(text);

  // ---- Account / sign in ----
  let username = $state('admin');
  let password = $state('');
  let confirm = $state('');
  let remember = $state(true);
  let busy = $state(false);
  let error = $state('');
  let showPw = $state(false);

  async function submit(e) {
    e.preventDefault();
    error = '';
    if (setup && password !== confirm) {
      error = "The passwords don't match.";
      return;
    }
    busy = true;
    try {
      const res = setup
        ? await api.post('/setup', { username, password })
        : await api.post('/login', { username, password, remember });
      Object.assign(session, { authenticated: true, setupRequired: false, username: res.username });
      if (setup) toast('Welcome to Homestack', 'success');
    } catch (err) {
      error = err.message;
      password = setup ? password : '';
    }
    busy = false;
  }

  const statusIcon = { ok: 'check-circle', warn: 'alert', fail: 'x-circle' };
</script>

<div class="wrap">
  <div class="brand">
    <svg width="36" height="36" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="8" fill="var(--accent)"/><path d="M9 12.5 16 9l7 3.5-7 3.5z" fill="var(--on-accent)"/><path d="m9 16.5 7 3.5 7-3.5" fill="none" stroke="var(--on-accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" opacity=".85"/><path d="m9 20.5 7 3.5 7-3.5" fill="none" stroke="var(--on-accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" opacity=".6"/></svg>
    <span>Homestack</span>
  </div>

  {#if setup && step === 'restore'}
    <div class="card panel wide">
      <button type="button" class="link back" onclick={() => (step = 'check')}><Icon name="chevron-left" size={14} /> Set up a new server instead</button>
      <h1>Restore from a backup</h1>
      <p class="muted lead">Bringing a server back after its boot drive died, or moving to a new one. Homestack's own account and settings come back now; the server's parts (the array, shares, schedules, VMs…) once the host agent is installed. RECOVERY.md has every step.</p>
      <RestoreBackup />
    </div>
  {:else if setup && step === 'check'}
    <div class="card panel wide">
      <div class="steps"><span class="on">1. Check your system</span><span>2. Create your account</span></div>
      <h1>Let's check everything's in place</h1>
      <p class="muted lead">Homestack needs Docker, Docker Compose and a folder to keep your stacks in.</p>

      {#if checkError}
        <p class="error-text">{checkError}</p>
      {/if}

      <ul class="checks">
        {#if !checks.length}
          {#each Array(5) as _}<li><div class="skeleton" style="height:18px;width:60%"></div></li>{/each}
        {/if}
        {#each checks as c (c.id)}
          <li class={c.status}>
            <Icon name={statusIcon[c.status]} size={18} />
            <div class="ctext">
              <strong>{c.label}</strong>
              <span class="muted">{c.detail}</span>
              {#if c.fix}
                <div class="fix">
                  <pre>{c.fix}</pre>
                  <button class="ghost icon sm" onclick={() => copy(c.fix)} aria-label="Copy"><Icon name="copy" size={14} /></button>
                </div>
              {/if}
            </div>
          </li>
        {/each}
      </ul>

      <div class="row foot">
        <button onclick={runChecks} disabled={checking}>
          <Icon name="refresh" size={15} class={checking ? 'spin' : ''} /> Check again
        </button>
        <span class="spacer"></span>
        {#if checks.length && failed}
          <button class="ghost" onclick={() => (step = 'account')}>Continue anyway</button>
        {/if}
        <button class="primary" disabled={!checks.length || failed > 0} onclick={() => (step = 'account')}>
          Continue <Icon name="chevron-right" size={15} />
        </button>
      </div>
      {#if checks.length}
        <p class="summary {failed ? 'bad' : warned ? 'meh' : 'good'}">
          {#if failed}{failed} {failed === 1 ? 'thing needs' : 'things need'} fixing before Homestack can manage your stacks.
          {:else if warned}Ready to go, with {warned} {warned === 1 ? 'suggestion' : 'suggestions'} above.
          {:else}Everything looks good.{/if}
        </p>
      {/if}
    </div>
  {:else}
    <form class="card panel" onsubmit={submit}>
      {#if setup}
        <div class="steps"><button type="button" class="link" onclick={() => (step = 'check')}>1. Check your system</button><span class="on">2. Create your account</span></div>
        <h1>Create your admin account</h1>
        <p class="muted lead">You'll use this to sign in. You can change it later in Settings.</p>
      {:else}
        <h1>Sign in</h1>
        <p class="muted lead">Welcome back.</p>
      {/if}

      <label class="field">
        <span>Username</span>
        <input bind:value={username} autocomplete="username" required autocapitalize="off" spellcheck="false" />
      </label>
      <label class="field">
        <span>Password</span>
        <div class="pw">
          <!-- svelte-ignore a11y_autofocus -->
          <input type={showPw ? 'text' : 'password'} bind:value={password} required minlength={setup ? 8 : undefined}
            autocomplete={setup ? 'new-password' : 'current-password'} autofocus={!setup} />
          <button type="button" class="ghost icon sm" onclick={() => (showPw = !showPw)} aria-label={showPw ? 'Hide password' : 'Show password'}>
            <Icon name={showPw ? 'eye-off' : 'eye'} size={15} />
          </button>
        </div>
        {#if setup}<span class="hint">At least 8 characters.</span>{/if}
      </label>
      {#if setup}
        <label class="field">
          <span>Confirm password</span>
          <input type={showPw ? 'text' : 'password'} bind:value={confirm} required autocomplete="new-password" />
        </label>
      {:else}
        <label class="remember">
          <input type="checkbox" bind:checked={remember} />
          <span>Keep me signed in on this device</span>
        </label>
      {/if}

      {#if error}<p class="error-text" role="alert">{error}</p>{/if}

      <button class="primary big" type="submit" disabled={busy}>
        {#if busy}<Icon name="loader" class="spin" size={16} />{/if}
        {setup ? 'Create account' : 'Sign in'}
      </button>
    </form>
    {#if !setup}
      <p class="forgot dim">Forgot your password? Run <code>homestack reset-password</code> on the server.</p>
    {/if}
  {/if}
  {#if setup && step !== 'restore'}
    <button type="button" class="link restore-link" onclick={() => (step = 'restore')}><Icon name="upgrade" size={14} /> Restoring a server? Restore from a backup instead</button>
  {/if}
  {#if session.source}
    <p class="legal dim">Homestack{session.version ? ` ${session.version}` : ''} is free software (<a href="https://www.gnu.org/licenses/agpl-3.0.html" target="_blank" rel="noopener">AGPL-3.0</a>) · <a href={sourceUrl(session.source, session.version)} target="_blank" rel="noopener">source code</a></p>
  {/if}
</div>

<style>
  .wrap { min-height: 100vh; display: flex; flex-direction: column; align-items: center; justify-content: center; padding: 32px 16px; gap: 20px; }
  .brand { display: flex; align-items: center; gap: 10px; font-weight: 650; font-size: 20px; letter-spacing: -0.02em; }
  .panel { width: 100%; max-width: 400px; padding: 28px; display: grid; gap: 16px; }
  .panel.wide { max-width: 640px; }
  h1 { font-size: 20px; }
  .lead { margin-top: -10px; }
  .steps { display: flex; gap: 14px; font-size: 12.5px; color: var(--text-3); }
  .steps .on { color: var(--accent-text); font-weight: 600; }
  .link { all: unset; cursor: pointer; }
  .link:hover { color: var(--text); }
  .checks { list-style: none; margin: 0; padding: 0; display: grid; gap: 2px; }
  .checks li { display: flex; gap: 12px; padding: 12px 4px; border-bottom: 1px solid var(--border); }
  .checks li:last-child { border-bottom: 0; }
  .checks li > :global(svg) { flex: none; margin-top: 1px; }
  .checks li.ok > :global(svg) { color: var(--green); }
  .checks li.warn > :global(svg) { color: var(--amber); }
  .checks li.fail > :global(svg) { color: var(--red); }
  .ctext { display: grid; gap: 2px; min-width: 0; flex: 1; }
  .ctext .muted { font-size: 13px; overflow-wrap: anywhere; }
  .fix { position: relative; margin-top: 8px; }
  .fix pre { margin: 0; padding: 10px 40px 10px 12px; background: var(--surface-2); border: 1px solid var(--border); border-radius: 8px; white-space: pre-wrap; font-size: 12px; color: var(--text); }
  .fix button { position: absolute; top: 5px; right: 5px; }
  .foot { padding-top: 4px; }
  .spacer { flex: 1; }
  .summary { font-size: 13px; margin-top: -4px; }
  .summary.good { color: var(--green-text); }
  .summary.meh { color: var(--amber-text); }
  .summary.bad { color: var(--red-text); }
  .pw { position: relative; }
  .pw input { padding-right: 38px; }
  .pw button { position: absolute; right: 3px; top: 3px; }
  .remember { display: flex; gap: 8px; align-items: center; font-size: 13.5px; color: var(--text-2); cursor: pointer; }
  .big { height: 40px; font-size: 14px; }
  .forgot { font-size: 12.5px; text-align: center; }
  .restore-link { display: inline-flex; gap: 6px; align-items: center; font-size: 13px; color: var(--text-2); }
  .back { display: inline-flex; gap: 4px; align-items: center; font-size: 13px; color: var(--text-2); }
  .legal { font-size: 12px; text-align: center; margin: 0; }
  .legal a { color: inherit; text-decoration: underline; }
</style>
