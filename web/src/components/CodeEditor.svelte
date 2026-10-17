<script>
  // CodeMirror, loaded only when an editor is first shown so the rest of the
  // app stays small.
  import { onMount } from 'svelte';

  let { value = $bindable(''), language = 'yaml', height = '420px', onsave, placeholder = '' } = $props();
  let host;
  let view;
  let loading = $state(true);

  // Languages, each loaded only when a file of that kind is first opened.
  // Beyond YAML and XML they're CodeMirror's legacy (stream) modes.
  const legacy = (mod, name) => async (StreamLanguage) => StreamLanguage.define((await mod())[name]);
  const languages = {
    yaml: async () => (await import('@codemirror/lang-yaml')).yaml(),
    xml: async () => (await import('@codemirror/lang-xml')).xml(),
    html: legacy(() => import('@codemirror/legacy-modes/mode/xml'), 'html'),
    json: legacy(() => import('@codemirror/legacy-modes/mode/javascript'), 'json'),
    javascript: legacy(() => import('@codemirror/legacy-modes/mode/javascript'), 'javascript'),
    typescript: legacy(() => import('@codemirror/legacy-modes/mode/javascript'), 'typescript'),
    python: legacy(() => import('@codemirror/legacy-modes/mode/python'), 'python'),
    shell: legacy(() => import('@codemirror/legacy-modes/mode/shell'), 'shell'),
    ini: legacy(() => import('@codemirror/legacy-modes/mode/properties'), 'properties'),
    toml: legacy(() => import('@codemirror/legacy-modes/mode/toml'), 'toml'),
    nginx: legacy(() => import('@codemirror/legacy-modes/mode/nginx'), 'nginx'),
    dockerfile: legacy(() => import('@codemirror/legacy-modes/mode/dockerfile'), 'dockerFile'),
    css: legacy(() => import('@codemirror/legacy-modes/mode/css'), 'css'),
    sql: legacy(() => import('@codemirror/legacy-modes/mode/sql'), 'standardSQL'),
    lua: legacy(() => import('@codemirror/legacy-modes/mode/lua'), 'lua'),
    go: legacy(() => import('@codemirror/legacy-modes/mode/go'), 'go'),
    rust: legacy(() => import('@codemirror/legacy-modes/mode/rust'), 'rust'),
    ruby: legacy(() => import('@codemirror/legacy-modes/mode/ruby'), 'ruby'),
    perl: legacy(() => import('@codemirror/legacy-modes/mode/perl'), 'perl'),
    diff: legacy(() => import('@codemirror/legacy-modes/mode/diff'), 'diff'),
    powershell: legacy(() => import('@codemirror/legacy-modes/mode/powershell'), 'powerShell'),
    jinja2: legacy(() => import('@codemirror/legacy-modes/mode/jinja2'), 'jinja2'),
  };

  onMount(() => {
    let destroyed = false;
    (async () => {
      const [{ EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection, placeholder: ph },
        { EditorState }, { defaultKeymap, history, historyKeymap, indentWithTab },
        { syntaxHighlighting, HighlightStyle, indentOnInput, bracketMatching, foldGutter, StreamLanguage },
        { tags }] = await Promise.all([
        import('@codemirror/view'), import('@codemirror/state'), import('@codemirror/commands'),
        import('@codemirror/language'), import('@lezer/highlight'),
      ]);
      const lang = languages[language] ? await languages[language](StreamLanguage) : null;
      if (destroyed) return;

      const highlight = HighlightStyle.define([
        { tag: [tags.propertyName, tags.definition(tags.propertyName)], color: 'var(--cm-key)' },
        { tag: [tags.string, tags.special(tags.string)], color: 'var(--cm-string)' },
        { tag: [tags.number, tags.bool, tags.null, tags.atom], color: 'var(--cm-number)' },
        { tag: [tags.comment, tags.lineComment], color: 'var(--cm-comment)', fontStyle: 'italic' },
        { tag: [tags.keyword, tags.typeName, tags.labelName, tags.meta, tags.tagName], color: 'var(--cm-keyword)' },
        { tag: [tags.attributeName], color: 'var(--cm-key)' },
        { tag: [tags.attributeValue], color: 'var(--cm-string)' },
        { tag: [tags.punctuation, tags.separator], color: 'var(--text-3)' },
        { tag: [tags.definition(tags.variableName), tags.function(tags.variableName), tags.className], color: 'var(--cm-key)' },
        { tag: [tags.standard(tags.variableName), tags.special(tags.variableName), tags.self], color: 'var(--cm-number)' },
        { tag: [tags.heading, tags.inserted], color: 'var(--cm-string)' },
        { tag: [tags.deleted], color: 'var(--cm-keyword)' },
      ]);

      // A tiny .env mode: comments, KEY=value
      const envLang = StreamLanguage.define({
        token(stream) {
          if (stream.sol() && stream.match(/^\s*#.*/)) return 'comment';
          if (stream.sol() && stream.match(/^\s*[A-Za-z_][A-Za-z0-9_.-]*(?==)/)) return 'propertyName';
          if (stream.match('=')) return 'punctuation';
          stream.skipToEnd();
          return 'string';
        },
      });

      const theme = EditorView.theme({
        '&': { height: '100%', fontSize: '13px', backgroundColor: 'var(--surface)', color: 'var(--text)' },
        '.cm-scroller': { fontFamily: 'var(--mono)', lineHeight: '1.6' },
        '.cm-content': { caretColor: 'var(--accent)', padding: '10px 0' },
        '.cm-gutters': { backgroundColor: 'var(--surface-2)', color: 'var(--text-3)', border: 'none', borderRight: '1px solid var(--border)' },
        '.cm-activeLine': { backgroundColor: 'color-mix(in srgb, var(--accent) 6%, transparent)' },
        '.cm-activeLineGutter': { backgroundColor: 'color-mix(in srgb, var(--accent) 10%, transparent)', color: 'var(--text)' },
        '&.cm-focused': { outline: 'none' },
        '.cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection': { backgroundColor: 'color-mix(in srgb, var(--accent) 25%, transparent) !important' },
        '.cm-cursor': { borderLeftColor: 'var(--accent)' },
        '.cm-foldGutter span': { color: 'var(--text-3)' },
        '.cm-placeholder': { color: 'var(--text-3)' },
      });

      const saveKey = {
        key: 'Mod-s',
        preventDefault: true,
        run: () => {
          onsave?.();
          return true;
        },
      };

      view = new EditorView({
        parent: host,
        state: EditorState.create({
          doc: value,
          extensions: [
            lineNumbers(),
            foldGutter(),
            highlightActiveLine(),
            highlightActiveLineGutter(),
            drawSelection(),
            history(),
            indentOnInput(),
            bracketMatching(),
            EditorState.tabSize.of(language === 'python' || language === 'go' ? 4 : 2),
            keymap.of([saveKey, indentWithTab, ...defaultKeymap, ...historyKeymap]),
            lang || (language === 'env' ? envLang : []),
            syntaxHighlighting(highlight),
            theme,
            placeholder ? ph(placeholder) : [],
            EditorView.lineWrapping,
            EditorView.updateListener.of((u) => {
              if (u.docChanged) value = u.state.doc.toString();
            }),
          ],
        }),
      });
      loading = false;
    })();
    return () => {
      destroyed = true;
      view?.destroy();
    };
  });

  // Replace the text when the value changes from outside (e.g. a reload).
  $effect(() => {
    const v = value;
    if (view && v !== view.state.doc.toString()) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: v } });
    }
  });
</script>

<div class="editor" style="height:{height}">
  <div class="host" bind:this={host}></div>
  {#if loading}<div class="loading">Loading editor…</div>{/if}
</div>

<style>
  .editor {
    position: relative; border: 1px solid var(--border-strong); border-radius: 10px; overflow: hidden; background: var(--surface);
    --cm-key: #4f46e5; --cm-string: #0f766e; --cm-number: #b45309; --cm-comment: #8a93a0; --cm-keyword: #be185d;
  }
  .editor:focus-within { border-color: color-mix(in srgb, var(--accent) 60%, var(--border)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 15%, transparent); }
  :global([data-theme='dark']) .editor { --cm-key: #a5abff; --cm-string: #5eead4; --cm-number: #fcd069; --cm-comment: #6d7584; --cm-keyword: #f9a8d4; }
  @media (prefers-color-scheme: dark) {
    :global(:root:not([data-theme='light'])) .editor { --cm-key: #a5abff; --cm-string: #5eead4; --cm-number: #fcd069; --cm-comment: #6d7584; --cm-keyword: #f9a8d4; }
  }
  .host { height: 100%; }
  .loading { position: absolute; inset: 0; display: grid; place-items: center; color: var(--text-3); }
</style>
