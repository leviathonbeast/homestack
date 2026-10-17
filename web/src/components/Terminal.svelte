<script>
  // An interactive terminal (xterm.js over a WebSocket): a shell inside a
  // container, or a VM's serial console (pass `path`).
  import { onMount } from 'svelte';

  let { containerId = '', shell = 'auto', user = '', path = '', onstatus, onclosed, describe = (reason) => reason || 'disconnected' } = $props();
  let host;
  let status = $state('connecting');

  onMount(() => {
    let term, ws, fit, ro, disposed = false;
    (async () => {
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/addon-fit'),
        import('@xterm/xterm/css/xterm.css'),
      ]);
      if (disposed) return;
      term = new Terminal({
        cursorBlink: true,
        fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
        fontSize: 13,
        lineHeight: 1.25,
        scrollback: 5000,
        theme: {
          background: '#0f1216',
          foreground: '#d6dae1',
          cursor: '#a5abff',
          selectionBackground: '#3b3f6b',
        },
      });
      fit = new FitAddon();
      term.loadAddon(fit);
      term.open(host);
      fit.fit();

      const proto = location.protocol === 'https:' ? 'wss' : 'ws';
      const size = new URLSearchParams({ cols: term.cols, rows: term.rows });
      let url;
      if (path) {
        url = `${proto}://${location.host}${path}${path.includes('?') ? '&' : '?'}${size}`;
      } else {
        const q = new URLSearchParams({ shell, cols: term.cols, rows: term.rows });
        if (user) q.set('user', user);
        url = `${proto}://${location.host}/api/containers/${encodeURIComponent(containerId)}/exec?${q}`;
      }
      ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      const enc = new TextEncoder();

      ws.onopen = () => {
        status = 'connected';
        onstatus?.(status);
        term.focus();
      };
      ws.onmessage = (e) => term.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data));
      ws.onclose = (e) => {
        status = 'closed';
        onstatus?.(status);
        onclosed?.(e.reason);
        term.write(`\r\n\x1b[90m[${describe(e.reason)}]\x1b[0m\r\n`);
      };
      term.onData((d) => ws.readyState === 1 && ws.send(enc.encode(d)));
      term.onResize(({ cols, rows }) => ws.readyState === 1 && ws.send(JSON.stringify({ type: 'resize', cols, rows })));
      ro = new ResizeObserver(() => {
        try {
          fit.fit();
        } catch {
          /* not visible */
        }
      });
      ro.observe(host);
    })();
    return () => {
      disposed = true;
      ro?.disconnect();
      ws?.close();
      term?.dispose();
    };
  });
</script>

<div class="term" bind:this={host}></div>

<style>
  .term { height: 100%; min-height: 360px; background: #0f1216; padding: 10px 0 10px 12px; border-radius: 10px; border: 1px solid var(--border); overflow: hidden; }
  .term :global(.xterm-viewport) { background: transparent !important; }
</style>
