<script>
  import { onMount, untrack } from 'svelte';
  import { api } from '../../lib/api.js';
  import Icon from '../../lib/Icon.svelte';
  import Modal from '../Modal.svelte';
  import CodeEditor from '../CodeEditor.svelte';

  // A systemd-networkd file, edited directly, for what the forms don't
  // cover. It goes through the same review, test and automatic undo.
  let { name = '', dir, onclose, onreview } = $props();

  // The form starts from the file it was opened for.
  const creating = untrack(() => !name);
  let fileName = $state(untrack(() => name));
  let content = $state('');
  let loaded = $state(creating);
  let error = $state('');

  const templates = {
    network: '[Match]\nName=\n\n[Network]\n',
    netdev: '[NetDev]\nName=\nKind=\n',
    link: '[Match]\nName=\n\n[Link]\n',
  };
  let type = $state('network');
  $effect(() => {
    if (creating && !content.replace(/\s/g, '').replace(/\[\w+\]|Name=|Kind=/g, '')) content = templates[type];
  });

  onMount(async () => {
    if (creating) return;
    try {
      content = (await api.get(`/network/host/file?name=${encodeURIComponent(name)}`)).content;
      loaded = true;
    } catch (e) {
      error = e.message;
    }
  });

  function review() {
    let n = fileName.trim();
    if (creating && n && !/\.(network|netdev|link)$/.test(n)) n += `.${type}`;
    if (!n) {
      error = 'Give the file a name, like 40-vlan50.network.';
      return;
    }
    onreview({ op: 'files', files: { [n]: content } });
  }
  function remove() {
    onreview({ op: 'files', files: { [name]: null } });
  }
</script>

<Modal title={creating ? 'New network file' : name} {onclose} width={820}>
  <p class="hint top">
    {#if creating}
      Files in <code>{dir}</code> are read in name order, so a number at the front (like <code>40-</code>) sets which comes first. A <code>.network</code> file needs a <code>[Match]</code> section saying which interface it's for.
    {:else}
      Edit <code>{dir}/{name}</code> directly. You'll see exactly what changes before anything is applied, and it's undone automatically unless you keep it.
    {/if}
    <a href="https://www.freedesktop.org/software/systemd/man/latest/systemd.network.html" target="_blank" rel="noreferrer">Settings reference <Icon name="external" size={12} /></a>
  </p>
  {#if creating}
    <div class="row">
      <label class="field grow"><span>File name</span><input class="mono" bind:value={fileName} placeholder="40-example.{type}" autocapitalize="off" spellcheck="false" /></label>
      <div class="field"><span>Kind</span>
        <div class="segmented">
          {#each ['network', 'netdev', 'link'] as t}<button type="button" class:active={type === t} onclick={() => (type = t)}>.{t}</button>{/each}
        </div>
      </div>
    </div>
  {/if}
  {#if loaded}
    <CodeEditor bind:value={content} language="ini" height="380px" onsave={review} />
  {:else if !error}
    <div class="skeleton" style="height:380px"></div>
  {/if}
  {#if error}<p class="error-text">{error}</p>{/if}
  {#snippet footer()}
    {#if !creating}<button class="danger" onclick={remove}><Icon name="trash" size={15} /> Delete…</button>{/if}
    <span class="spacer"></span>
    <button onclick={onclose}>Cancel</button>
    <button class="primary" onclick={review} disabled={!loaded}><Icon name="eye" size={15} /> Review the change</button>
  {/snippet}
</Modal>

<style>
  .top { margin: 0 0 12px; line-height: 1.6; }
  .top a { display: inline-flex; gap: 4px; align-items: center; }
  .row { display: flex; gap: 12px; align-items: end; margin-bottom: 12px; flex-wrap: wrap; }
  .grow { flex: 1; min-width: 220px; }
  .field { display: grid; gap: 6px; }
  .field > span:first-child { font-size: 13px; font-weight: 500; }
  .mono { font-family: var(--mono, ui-monospace, monospace); }
  .spacer { flex: 1; }
</style>
