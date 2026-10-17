<script>
  import { dialog } from '../lib/state.svelte.js';
  import Modal from './Modal.svelte';

  let typed = $state('');
  const d = $derived(dialog.current);
  const canConfirm = $derived(!d?.typeToConfirm || typed === d.typeToConfirm);

  function close(result) {
    const cur = dialog.current;
    dialog.current = null;
    typed = '';
    if (!cur) return;
    if (result && cur.options.length) {
      const out = {};
      for (const o of cur.options) out[o.key] = o.checked;
      cur.resolve(out);
    } else {
      cur.resolve(result);
    }
  }
</script>

{#if d}
  <Modal title={d.title} onclose={() => close(false)}>
    {#if d.message}<p class="msg">{d.message}</p>{/if}
    {#each d.options as opt (opt.key)}
      <label class="opt">
        <input type="checkbox" bind:checked={opt.checked} />
        <span>{opt.label}{#if opt.hint}<small>{opt.hint}</small>{/if}</span>
      </label>
    {/each}
    {#if d.typeToConfirm}
      <label class="field type">
        <span>Type <code>{d.typeToConfirm}</code> to confirm</span>
        <!-- svelte-ignore a11y_autofocus -->
        <input bind:value={typed} autofocus autocomplete="off" spellcheck="false"
          onkeydown={(e) => e.key === 'Enter' && canConfirm && close(true)} />
      </label>
    {/if}
    {#snippet footer()}
      <button onclick={() => close(false)}>Cancel</button>
      <!-- svelte-ignore a11y_autofocus -->
      <button class={d.danger ? 'danger-fill' : 'primary'} disabled={!canConfirm} onclick={() => close(true)} autofocus={!d.typeToConfirm}>
        {d.confirmLabel}
      </button>
    {/snippet}
  </Modal>
{/if}

<style>
  .msg { line-height: 1.55; white-space: pre-line; }
  .opt { display: flex; gap: 10px; align-items: flex-start; margin-top: 14px; color: var(--text); cursor: pointer; }
  .opt input { margin-top: 2px; }
  .opt small { display: block; color: var(--text-3); font-size: 12.5px; }
  .type { margin-top: 16px; }
</style>
