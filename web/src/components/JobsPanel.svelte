<script>
  // Shows the live output of stack actions and image pulls. It stays put while
  // you move around the app, so a long deploy doesn't block you.
  import { jobs } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';

  let selected = $state(null);
  let out = $state();
  let stick = true;

  const job = $derived(jobs.list.find((j) => j.id === selected) || jobs.list[0]);
  const running = $derived(jobs.list.filter((j) => j.status === 'running').length);

  // Follow the newest job whenever one starts.
  $effect(() => {
    if (jobs.list[0]) selected = jobs.list[0].id;
  });

  $effect(() => {
    job?.lines.length;
    if (out && stick) queueMicrotask(() => (out.scrollTop = out.scrollHeight));
  });

  function onscroll() {
    stick = out.scrollHeight - out.scrollTop - out.clientHeight < 30;
  }
</script>

{#if jobs.list.length}
  {#if jobs.open && job}
    <section class="panel" aria-label="Action output">
      <header>
        {#if job.status === 'running'}
          <Icon name="loader" class="spin s-run" size={16} />
        {:else if job.status === 'ok'}
          <Icon name="check-circle" class="s-ok" size={16} />
        {:else}
          <Icon name="x-circle" class="s-err" size={16} />
        {/if}
        {#if jobs.list.length > 1}
          <select bind:value={selected} aria-label="Choose an action">
            {#each jobs.list as j (j.id)}
              <option value={j.id}>{j.title}{j.status === 'running' ? ' (running)' : ''}</option>
            {/each}
          </select>
        {:else}
          <strong class="truncate">{job.title}</strong>
        {/if}
        <span class="spacer"></span>
        <button class="ghost icon sm" onclick={() => (jobs.open = false)} aria-label="Minimise"><Icon name="minimize" /></button>
        {#if job.status !== 'running'}
          <button class="ghost icon sm" aria-label="Close" onclick={() => { jobs.list.splice(jobs.list.indexOf(job), 1); }}><Icon name="x" /></button>
        {/if}
      </header>
      <pre bind:this={out} {onscroll}>{#each job.lines as l}{l}{'\n'}{/each}{#if job.status === 'running'}<span class="cursor">▍</span>{/if}</pre>
      {#if job.status === 'error'}
        <div class="err"><Icon name="alert" size={14} /> {job.error}</div>
      {/if}
    </section>
  {:else}
    <button class="pill" onclick={() => (jobs.open = true)}>
      {#if running}
        <Icon name="loader" class="spin" size={14} /> {running} running
      {:else}
        <Icon name="terminal" size={14} /> Output
      {/if}
    </button>
  {/if}
{/if}

<style>
  .panel {
    position: fixed; right: 16px; bottom: 16px; z-index: 90; width: min(560px, calc(100vw - 32px));
    background: var(--surface); border: 1px solid var(--border); border-radius: 12px;
    box-shadow: var(--shadow-lg); overflow: hidden; animation: up 0.16s ease-out;
  }
  header { display: flex; align-items: center; gap: 8px; padding: 8px 8px 8px 14px; border-bottom: 1px solid var(--border); }
  header strong { font-size: 13.5px; }
  header select { height: 28px; width: auto; max-width: 70%; font-size: 13px; }
  .spacer { flex: 1; }
  pre {
    margin: 0; height: 240px; overflow: auto; padding: 12px 14px; background: var(--log-bg); color: var(--log-text);
    font-size: 12px; line-height: 1.55; white-space: pre-wrap; word-break: break-word;
  }
  .cursor { color: var(--log-dim); animation: blink 1s steps(2) infinite; }
  .err { display: flex; gap: 6px; align-items: center; padding: 9px 14px; font-size: 13px; color: var(--red-text); background: var(--red-soft); }
  .pill {
    position: fixed; right: 16px; bottom: 16px; z-index: 90; border-radius: 999px; box-shadow: var(--shadow-lg);
  }
  :global(.s-run) { color: var(--accent); }
  :global(.s-ok) { color: var(--green); }
  :global(.s-err) { color: var(--red); }
  @keyframes up { from { opacity: 0; transform: translateY(8px); } }
  @keyframes blink { 50% { opacity: 0; } }
</style>
