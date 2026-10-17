<script>
  // First-boot settings for a VM made from a cloud-init template: the
  // hostname, the login and the network. The VM reads them from a small
  // setup disk the first time it starts.
  import Icon from '../lib/Icon.svelte';

  let { ci = $bindable(), vmName = '', hasNIC = true } = $props();
  let show = $state(false);

  // Mirrors cloudinit.HostnameFrom on the server.
  const hostDefault = $derived(
    vmName.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63).replace(/-+$/, ''),
  );
  const keys = $derived(ci.sshKeys.split('\n').filter((l) => l.trim() && !l.trim().startsWith('#')));
  const privateKey = $derived(/PRIVATE KEY/.test(ci.sshKeys));
  const noLogin = $derived(!keys.length && !ci.password);
</script>

<section class="card">
  <div class="card-header"><h2>First boot</h2><span class="dim small">Applied by cloud-init</span></div>
  <div class="card-body stack-v">
    <div class="grid2">
      <label class="field"><span>Hostname</span>
        <input class="mono" bind:value={ci.hostname} placeholder={hostDefault || 'web01'} autocapitalize="off" spellcheck="false" />
        <span class="hint">Defaults to the VM's name. Add a domain for a full name, like web01.home.lan.</span></label>
      <label class="field"><span>Username</span>
        <input class="mono" bind:value={ci.user} required placeholder="admin" autocapitalize="off" spellcheck="false" autocomplete="off" />
        <span class="hint">Your account in the VM, with sudo. Root logins stay off.</span></label>
    </div>

    <label class="field"><span>SSH public keys</span>
      <textarea class="mono" rows="3" bind:value={ci.sshKeys} placeholder="ssh-ed25519 AAAA… you@pc" autocapitalize="off" spellcheck="false"></textarea>
      <span class="hint">One per line: the contents of a <code>.pub</code> file, like <code>~/.ssh/id_ed25519.pub</code>. Remembered for next time.</span></label>
    {#if privateKey}
      <div class="note red"><Icon name="alert" size={16} /><span>That's a <strong>private</strong> key. Remove it and paste the public key (the <code>.pub</code> file) instead; never share the private key.</span></div>
    {/if}

    <div class="grid2">
      <label class="field"><span>Password <small class="dim">(optional)</small></span>
        <div class="pw">
          <input type={show ? 'text' : 'password'} bind:value={ci.password} minlength="8" autocomplete="new-password" placeholder={keys.length ? 'Not needed with an SSH key' : 'At least 8 characters'} />
          <button type="button" class="ghost icon sm" onclick={() => (show = !show)} aria-label={show ? 'Hide password' : 'Show password'}><Icon name={show ? 'eye-off' : 'eye'} size={15} /></button>
        </div>
        <span class="hint">For the console and sudo. Stored in the VM only as a hash. Without one, sudo needs no password and you log in with your key.</span></label>
      <div class="field"><span>&nbsp;</span>
        <label class="chk"><input type="checkbox" bind:checked={ci.sshPassword} disabled={!ci.password} /> <span>Also allow password logins over SSH <small class="dim">(keys are safer)</small></span></label>
      </div>
    </div>

    {#if noLogin}
      <div class="note amber"><Icon name="alert" size={16} /><span>Add an SSH key or a password, or nobody will be able to log in to the VM.</span></div>
    {/if}

    {#if hasNIC}
      <div class="field"><span>Network address</span>
        <div class="segmented">
          <button type="button" class:active={ci.network.mode === 'dhcp'} onclick={() => (ci.network.mode = 'dhcp')}>Automatic (DHCP)</button>
          <button type="button" class:active={ci.network.mode === 'static'} onclick={() => (ci.network.mode = 'static')}>Fixed address</button>
        </div>
      </div>
      {#if ci.network.mode === 'static'}
        <div class="grid3">
          <label class="field"><span>Address / prefix</span><input class="mono" bind:value={ci.network.address} required placeholder="192.168.1.50/24" autocapitalize="off" spellcheck="false" /></label>
          <label class="field"><span>Gateway</span><input class="mono" bind:value={ci.network.gateway} placeholder="192.168.1.1" autocapitalize="off" spellcheck="false" /></label>
          <label class="field"><span>DNS servers</span><input class="mono" bind:value={ci.network.dns} placeholder="Same as the gateway" autocapitalize="off" spellcheck="false" /></label>
        </div>
        <span class="hint">Pick an address outside your router's DHCP pool, or reserve it there, so nothing else gets it.</span>
      {/if}
    {/if}

    <div class="checks">
      <label class="chk"><input type="checkbox" bind:checked={ci.upgrade} /> <span>Install updates on first boot <small class="dim">(recommended: images can be weeks old)</small></span></label>
      <label class="chk"><input type="checkbox" bind:checked={ci.guestAgent} /> <span>Install the QEMU guest agent <small class="dim">(clean shutdowns and IP reporting)</small></span></label>
    </div>
  </div>
</section>

<style>
  .grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
  .grid3 { display: grid; grid-template-columns: 1.3fr 1fr 1fr; gap: 12px; }
  .field { display: grid; gap: 6px; align-content: start; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .pw { display: flex; gap: 4px; align-items: center; }
  .pw input { flex: 1; }
  .chk { display: flex; gap: 8px; align-items: flex-start; font-size: 13.5px; cursor: pointer; }
  .chk input { margin-top: 3px; }
  .checks { display: grid; gap: 8px; }
  .small { font-size: 12.5px; }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; border-radius: 10px; font-size: 13px; line-height: 1.5; }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note.red { background: var(--red-soft); color: var(--red-text); }
  @media (max-width: 700px) { .grid2, .grid3 { grid-template-columns: 1fr; } }
</style>
