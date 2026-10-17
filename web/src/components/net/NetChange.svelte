<script>
  import { onMount } from 'svelte';
  import { api } from '../../lib/api.js';
  import { lineDiff } from '../../lib/diff.js';
  import Icon from '../../lib/Icon.svelte';
  import Modal from '../Modal.svelte';

  // The review screen every change to the server's own network goes
  // through: exactly what changes, what to watch out for, and how the
  // safety net works. Only then does it apply.
  let { op, onclose, ondone } = $props();

  let plan = $state(null);
  let error = $state('');
  let failed = $state(''); // it was applied, failed its test, and was undone
  let applying = $state(false);
  let typed = $state('');

  onMount(() => {
    api.post('/network/host/plan', op).then((p) => (plan = p)).catch((e) => (error = e.message));
  });

  const ready = $derived(plan && (!plan.confirmWord || typed.trim() === plan.confirmWord));
  const ops = $derived(Object.entries(plan?.linkOps || {}));

  async function apply() {
    applying = true;
    error = '';
    try {
      const pending = await api.post('/network/host/apply', { ...op, confirmText: typed.trim() });
      ondone?.(pending);
    } catch (e) {
      if (/undone/i.test(e.message)) failed = e.message;
      else error = e.message;
    }
    applying = false;
  }

  // Show every diff unless they're long; reviewing them is the point.
  const openAll = $derived((plan?.files || []).reduce((n, f) => n + (f.before + f.after).split('\n').length, 0) <= 240);

  const kind = (f) => (!f.before ? 'New file' : !f.after ? 'Deleted' : 'Changed');
</script>

<Modal title="Review the change" {onclose} width={780} dismissable={!applying}>
  {#if failed}
    <div class="note red"><Icon name="alert" size={16} /><div><strong>It was undone, so nothing changed.</strong><br />{failed}</div></div>
  {:else if !plan && !error}
    <div class="skeleton" style="height:160px"></div>
  {:else if plan}
    <h3 class="sum">{plan.summary}</h3>

    {#each plan.notes as n, i}
      <div class="note" class:red={i === 0 && plan.critical} class:amber={!(i === 0 && plan.critical)}>
        <Icon name={i === 0 && plan.critical ? 'alert' : 'info'} size={16} /><span>{n}</span>
      </div>
    {/each}

    {#each ops as [link, up]}
      <p class="what"><Icon name={up ? 'play' : 'stop'} size={14} /> {up ? 'Brings up' : 'Takes down'} <strong>{link}</strong>.</p>
    {/each}
    {#if plan.remove?.length}
      <p class="what"><Icon name="trash" size={14} /> Deletes the interface{plan.remove.length > 1 ? 's' : ''} <strong>{plan.remove.join(', ')}</strong>.</p>
    {/if}

    {#if plan.files?.length}
      <h4>What changes in /etc/systemd/network</h4>
      <div class="files">
        {#each plan.files as f (f.name)}
          <details open={openAll || plan.files.length === 1}>
            <summary><span class="badge" class:green={!f.before} class:red={!f.after}>{kind(f)}</span> <code>{f.name}</code></summary>
            <div class="diff">
              {#each lineDiff(f.before, f.after) as r}
                <div class="l" class:add={r.t === '+'} class:del={r.t === '-'} class:fold={r.t === '…'}>
                  <span class="n">{r.a ?? ''}</span><span class="n">{r.b ?? ''}</span><span class="m">{r.t === '…' ? '' : r.t}</span><span class="t">{r.text}</span>
                </div>
              {/each}
            </div>
          </details>
        {/each}
      </div>
    {/if}

    <div class="safety">
      <Icon name="shield" size={18} />
      <div>
        <strong>How you're protected</strong>
        <ol>
          <li>A backup of the network files is saved first.</li>
          <li>After applying, each interface must still reach its gateway. If one can't, the change is undone straight away.</li>
          <li>You then have {plan.seconds} seconds to keep it. If this page stops responding, you can't keep it, so it's undone automatically.</li>
        </ol>
      </div>
    </div>

    {#if plan.confirmWord}
      <label class="field confirm"><span>Type <code>{plan.confirmWord}</code> to apply</span>
        <input bind:value={typed} autocapitalize="off" autocomplete="off" spellcheck="false" placeholder={plan.confirmWord} /></label>
    {/if}
  {/if}
  {#if error}<div class="note red"><Icon name="alert" size={16} /><span>{error}</span></div>{/if}

  {#snippet footer()}
    {#if failed || (error && !plan)}
      <button class="primary" onclick={onclose}>Close</button>
    {:else}
      <button onclick={onclose} disabled={applying}>Cancel</button>
      <button class="primary" disabled={!ready || applying} onclick={apply}>
        {#if applying}<Icon name="loader" size={15} class="spin" /> Applying and testing…{:else}<Icon name="check" size={15} /> Apply{/if}
      </button>
    {/if}
  {/snippet}
</Modal>

<style>
  .sum { font-size: 15.5px; margin-bottom: 12px; }
  h4 { font-size: 13px; font-weight: 600; margin: 16px 0 8px; color: var(--text-2); }
  .note { display: flex; gap: 10px; align-items: flex-start; padding: 10px 13px; border-radius: 10px; margin-bottom: 8px; font-size: 13.5px; line-height: 1.5; }
  .note :global(svg) { flex: none; margin-top: 2px; }
  .note.amber { background: var(--amber-soft); color: var(--amber-text); }
  .note.red { background: var(--red-soft); color: var(--red-text); }
  .what { display: flex; gap: 8px; align-items: center; font-size: 13.5px; margin: 6px 0; }
  .files { display: grid; gap: 8px; }
  details { border: 1px solid var(--border); border-radius: 10px; overflow: hidden; }
  summary { display: flex; gap: 8px; align-items: center; padding: 8px 12px; cursor: pointer; background: var(--surface-2); font-size: 13px; }
  .diff { font-family: var(--mono, ui-monospace, monospace); font-size: 12.5px; line-height: 1.55; max-height: 340px; overflow: auto; }
  .l { display: grid; grid-template-columns: 34px 34px 16px 1fr; white-space: pre; }
  .l .n { color: var(--text-3); text-align: right; padding-right: 6px; user-select: none; }
  .l .m { user-select: none; text-align: center; }
  .l.add { background: var(--green-soft); color: var(--green-text); }
  .l.del { background: var(--red-soft); color: var(--red-text); }
  .l.fold { color: var(--text-3); font-style: italic; background: var(--surface-2); }
  .safety { display: flex; gap: 12px; padding: 12px 14px; margin-top: 14px; border-radius: 10px; background: var(--accent-soft); color: var(--accent-text); font-size: 13px; line-height: 1.5; }
  .safety :global(svg) { flex: none; margin-top: 1px; }
  .safety ol { margin: 4px 0 0 18px; padding: 0; }
  .confirm { margin-top: 14px; display: grid; gap: 6px; }
  .confirm > span { font-size: 13px; font-weight: 500; }
  .confirm input { font-family: var(--mono, ui-monospace, monospace); max-width: 260px; }
</style>
