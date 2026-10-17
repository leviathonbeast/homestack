<script>
  import { untrack } from 'svelte';
  import Icon from '../../lib/Icon.svelte';
  import Modal from '../Modal.svelte';

  // The settings of one interface's .network file, as a form. Nothing is
  // changed here: "Review" shows exactly what would change in the file first.
  let { link, via, onclose, onreview } = $props();

  // The form starts from the settings it was opened with.
  const c = untrack(() => link.config);
  const port = !!c.port;
  const portOf = port ? c.port.split(' ')[1] : '';
  let dhcp = $state(c.dhcp || 'no');
  let addrs = $state(c.addresses.map((a) => ({ was: a, now: a })));
  let gateway4 = $state(c.gateway4 || '');
  let gateway6 = $state(c.gateway6 || '');
  let dns = $state(c.dns.join(', '));
  let mtu = $state(c.mtu || '');

  const yoursAddr = (a) => link.yours && a.was && a.was.split('/')[0] === via;

  function review(e) {
    e.preventDefault();
    onreview({
      op: 'link-config',
      link: link.name,
      config: {
        dhcp,
        addresses: addrs.map((a) => ({ was: a.was, now: a.now.trim() })).filter((a) => a.was || a.now),
        gateway4: gateway4.trim(),
        gateway6: gateway6.trim(),
        dns: dns.split(/[\s,]+/).filter(Boolean),
        mtu: +mtu || 0,
      },
    });
  }
</script>

<Modal title="Settings for {link.name}" {onclose} width={620}>
  <form id="linkform" class="stack-v" onsubmit={review}>
    <p class="hint top">These are the settings in <code>{c.file}</code>. Nothing changes until you've reviewed the exact edit to the file and applied it.</p>
    {#if link.yours}
      <div class="note"><Icon name="info" size={16} /><span>You're connected to Homestack through {link.name} ({via}). That address is locked here: changing it would cut this page off. To move it, add the new address, open Homestack on it, then come back and remove the old one.</span></div>
    {/if}

    {#if port}
      <div class="note"><Icon name="info" size={16} /><span>{link.name} is part of {portOf}, so it carries no addresses of its own: those are set on {portOf}. Only the MTU applies here.</span></div>
    {:else}
      <div class="field"><span>How it gets its address</span>
        <div class="segmented">
          <button type="button" class:active={dhcp === 'no'} onclick={() => (dhcp = 'no')}>Fixed (static)</button>
          <button type="button" class:active={dhcp === 'yes'} onclick={() => (dhcp = 'yes')}>From the router (DHCP)</button>
          {#if c.dhcp === 'ipv4' || c.dhcp === 'ipv6'}
            <button type="button" class:active={dhcp === c.dhcp} onclick={() => (dhcp = c.dhcp)}>DHCP, {c.dhcp === 'ipv4' ? 'IPv4' : 'IPv6'} only</button>
          {/if}
        </div>
        <span class="hint">A server is easiest to find with a fixed address. DHCP lets the router choose one, which can change.</span>
      </div>

      <div class="field"><span>Addresses</span>
        {#each addrs as a, i}
          <div class="row">
            <input class="mono grow" bind:value={a.now} placeholder="10.0.10.5/24" disabled={yoursAddr(a)} autocapitalize="off" spellcheck="false" />
            {#if yoursAddr(a)}
              <span class="lock" title="Your connection"><Icon name="lock" size={15} /></span>
            {:else}
              <button type="button" class="ghost icon" aria-label="Remove" title="Remove" onclick={() => (addrs = addrs.filter((_, k) => k !== i))}><Icon name="x" size={15} /></button>
            {/if}
          </div>
        {/each}
        <button type="button" class="add" onclick={() => (addrs = [...addrs, { was: '', now: '' }])}><Icon name="plus" size={14} /> Add an address</button>
        <span class="hint">An address with its network size, like <code>10.0.10.5/24</code> (/24 means the first three numbers are the network). Leave none and use DHCP to let the router decide.</span>
      </div>

      <div class="grid2">
        <label class="field"><span>Gateway (IPv4)</span>
          <input class="mono" bind:value={gateway4} placeholder="10.0.10.1" autocapitalize="off" spellcheck="false" />
          <span class="hint">Your router: where traffic for the internet goes. It must be on one of the networks above.</span></label>
        <label class="field"><span>Gateway (IPv6)</span>
          <input class="mono" bind:value={gateway6} placeholder="optional" autocapitalize="off" spellcheck="false" />
          <span class="hint">Often the router's fe80:: address.</span></label>
      </div>

      <label class="field"><span>DNS servers</span>
        <input class="mono" bind:value={dns} placeholder="1.1.1.1, 9.9.9.9" autocapitalize="off" spellcheck="false" />
        <span class="hint">Who turns names into addresses, separated by commas. Leave empty to use what the router hands out.</span></label>
    {/if}

    <label class="field"><span>MTU</span>
      <input type="number" min="1280" max="9216" bind:value={mtu} placeholder="1500 (standard)" style="max-width:200px" />
      <span class="hint">The largest packet size. Leave empty for the standard 1500. 9000 (jumbo frames) only works if every switch and device on the way supports it.</span></label>

    {#if c.policy}
      <div class="note soft"><Icon name="info" size={16} /><span>This file also has {c.policy} policy routing setting{c.policy > 1 ? 's' : ''} (rules and routes in other tables). They're kept as they are; if the gateway changes, it changes in them too, so they keep matching.</span></div>
    {/if}
  </form>
  {#snippet footer()}
    <button onclick={onclose}>Cancel</button>
    <button class="primary" type="submit" form="linkform"><Icon name="eye" size={15} /> Review the change</button>
  {/snippet}
</Modal>

<style>
  .top { margin: 0; }
  .field { display: grid; gap: 6px; align-content: start; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .row { display: flex; gap: 8px; align-items: center; }
  .grow { flex: 1; }
  .grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; align-items: start; }
  .add { justify-self: start; height: 30px; font-size: 13px; }
  .lock { width: 34px; display: grid; place-items: center; color: var(--text-3); }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 13px; border-radius: 10px; font-size: 13px; line-height: 1.5; background: var(--accent-soft); color: var(--accent-text); }
  .note.soft { background: var(--surface-2); color: var(--text-2); }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  @media (max-width: 640px) { .grid2 { grid-template-columns: 1fr; } }
</style>
