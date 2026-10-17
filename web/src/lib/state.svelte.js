// App-wide state, using Svelte 5 runes.
import { stream } from './api.js';

export const session = $state({
  loaded: false,
  authenticated: false,
  setupRequired: false,
  username: '',
  version: '',
  source: '', // where this build's source code is (Homestack is AGPL)
  appHost: '', // replaces [HOST] in app links (Settings → Stacks and VMs)
});

// Optional features the server can offer (VMs need libvirt on the host).
export const features = $state({ vms: false, vmsChecked: false, host: false });

// ---------- Theme ----------

export const theme = $state({ value: 'system' });

export function applyTheme(value) {
  theme.value = value;
  const root = document.documentElement;
  if (value === 'light' || value === 'dark') root.dataset.theme = value;
  else delete root.dataset.theme;
  try {
    localStorage.setItem('homestack-theme', value);
  } catch {
    /* private mode */
  }
}

// ---------- Toasts ----------

export const toasts = $state({ list: [] });
let toastId = 0;

export function toast(message, kind = 'info', ms = 4000) {
  const id = ++toastId;
  toasts.list.push({ id, message, kind });
  if (ms) setTimeout(() => dismissToast(id), ms);
  return id;
}

export function dismissToast(id) {
  const i = toasts.list.findIndex((t) => t.id === id);
  if (i >= 0) toasts.list.splice(i, 1);
}

// ---------- Live Docker events ----------

// `live.tick` increases whenever Docker reports a change, so pages can reload.
export const live = $state({ tick: 0, connected: false });
let tickTimer;

export function bumpLive() {
  clearTimeout(tickTimer);
  tickTimer = setTimeout(() => live.tick++, 350);
}

// ---------- Jobs (stack actions, pulls) ----------

// Jobs keep running while you move around the app; the console panel shows them.
export const jobs = $state({ list: [], open: false });
let jobId = 0;

export async function runJob(title, method, path, body) {
  const job = { id: ++jobId, title, lines: [], status: 'running', error: '', started: Date.now() };
  jobs.list.unshift(job);
  if (jobs.list.length > 8) jobs.list.length = 8;
  jobs.open = true;
  const ref = jobs.list[0];
  const res = await stream(method, path, body, (line) => {
    ref.lines.push(line);
    if (ref.lines.length > 3000) ref.lines.splice(0, ref.lines.length - 3000);
  });
  ref.status = res.ok ? 'ok' : 'error';
  ref.error = res.error || '';
  if (res.ok) toast(`${title}: done`, 'success');
  else toast(`${title}: ${res.error}`, 'error', 8000);
  bumpLive();
  return res;
}

// ---------- Confirm dialog ----------

export const dialog = $state({ current: null });

/**
 * Ask the user to confirm. Resolves true/false (or the checkbox state object
 * when `options` are given).
 */
export function confirm(opts) {
  return new Promise((resolve) => {
    dialog.current = {
      title: opts.title,
      message: opts.message || '',
      confirmLabel: opts.confirmLabel || 'Confirm',
      danger: !!opts.danger,
      typeToConfirm: opts.typeToConfirm || '',
      options: (opts.options || []).map((o) => ({ ...o, checked: !!o.checked })),
      resolve,
    };
  });
}
