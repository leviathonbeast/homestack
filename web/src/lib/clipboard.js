import { toast } from './state.svelte.js';

// Copies text and says so. navigator.clipboard only exists on https and
// localhost, and Homestack is usually opened over plain http on the LAN, so
// it falls back to the older copy command there instead of failing silently.
export async function copy(text, what = 'Copied') {
  let ok = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      ok = true;
    }
  } catch {
    ok = false;
  }
  if (!ok) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0;pointer-events:none';
    document.body.appendChild(ta);
    ta.select();
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
  }
  if (ok) toast(what, 'success', 1500);
  else toast("Couldn't copy here: select the text and press Ctrl+C.", 'warn', 4000);
  return ok;
}
