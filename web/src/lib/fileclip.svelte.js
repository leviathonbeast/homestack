// The file browser's clipboard: what Copy or Cut picked, kept while you open
// the folder to paste into, like a desktop file manager. It's kept for the
// tab (sessionStorage), so a page refresh between Cut and Paste doesn't
// lose it; where storage is blocked it simply lasts until the page reloads.
const KEY = 'homestack.fileclip';

function load() {
  try {
    const v = JSON.parse(sessionStorage.getItem(KEY) || 'null');
    if (v && (v.op === 'copy' || v.op === 'move') && typeof v.from === 'string' && Array.isArray(v.names)) return v;
  } catch {
    // unavailable or damaged: start empty
  }
  return { op: '', from: '', names: [] };
}

export const clip = $state(load()); // op: copy | move

function save() {
  try {
    if (clip.op) sessionStorage.setItem(KEY, JSON.stringify(clip));
    else sessionStorage.removeItem(KEY);
  } catch {
    // not kept across reloads, that's all
  }
}

export function setClip(op, from, names) {
  clip.op = op;
  clip.from = from;
  clip.names = names;
  save();
}

export function clearClip() {
  clip.op = '';
  clip.from = '';
  clip.names = [];
  save();
}
