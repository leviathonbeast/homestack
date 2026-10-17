import { session } from './state.svelte.js';

export function bytes(n, digits = 1) {
  if (n == null || isNaN(n)) return '–';
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let v = n;
  let i = -1;
  do {
    v /= 1024;
    i++;
  } while (v >= 1024 && i < units.length - 1);
  return `${v.toFixed(v >= 100 ? 0 : digits)} ${units[i]}`;
}

/** Bytes per second as a data rate, e.g. "45.2 MB/s". */
export function rate(n) {
  return n == null || isNaN(n) ? '–' : `${bytes(Math.round(n))}/s`;
}

/** Bytes per second in bits, as network speeds are given, e.g. "84.1 Mb/s". */
export function bitrate(n) {
  if (n == null || isNaN(n)) return '–';
  const b = n * 8;
  if (b < 1000) return `${Math.round(b)} b/s`;
  const units = ['Kb/s', 'Mb/s', 'Gb/s', 'Tb/s'];
  let v = b;
  let i = -1;
  do {
    v /= 1000;
    i++;
  } while (v >= 1000 && i < units.length - 1);
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`;
}

/** A link speed in Mb/s, e.g. 10000 -> "10G". */
export function linkSpeed(mbps) {
  if (!mbps) return '';
  return mbps >= 1000 ? `${+(mbps / 1000).toFixed(1)}G` : `${mbps}M`;
}

export function percent(n, digits = 1) {
  if (n == null || isNaN(n)) return '–';
  return `${n.toFixed(n >= 100 ? 0 : digits)}%`;
}

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

/** "3 hours ago" from a unix timestamp (seconds) or a date string. */
export function ago(t) {
  if (!t) return '–';
  const ms = typeof t === 'number' ? t * 1000 : Date.parse(t);
  if (isNaN(ms)) return '–';
  const diff = (ms - Date.now()) / 1000;
  const abs = Math.abs(diff);
  if (abs < 45) return 'just now';
  if (abs < 3600) return rtf.format(Math.round(diff / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), 'hour');
  if (abs < 86400 * 30) return rtf.format(Math.round(diff / 86400), 'day');
  if (abs < 86400 * 365) return rtf.format(Math.round(diff / (86400 * 30)), 'month');
  return rtf.format(Math.round(diff / (86400 * 365)), 'year');
}

export function dateTime(t) {
  if (!t) return '–';
  const ms = typeof t === 'number' ? t * 1000 : Date.parse(t);
  if (isNaN(ms) || ms <= 0) return '–';
  return new Date(ms).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

export function duration(sec) {
  if (!sec) return '–';
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}

export function shortId(id) {
  return (id || '').replace(/^sha256:/, '').slice(0, 12);
}

/** Status dot color for a stack or container state. */
export function stateColor(state, health) {
  if (health === 'unhealthy') return 'red';
  switch (state) {
    case 'running':
      return health === 'starting' ? 'amber' : 'green';
    case 'partial':
    case 'restarting':
    case 'paused':
      return 'amber';
    case 'dead':
      return 'red';
    default:
      return '';
  }
}

export function stackStatusLabel(s) {
  return { running: 'Running', partial: 'Partly running', stopped: 'Stopped', inactive: 'Not deployed' }[s] || s;
}

export function containerStateLabel(state) {
  return (
    {
      running: 'Running',
      exited: 'Stopped',
      created: 'Created',
      paused: 'Paused',
      restarting: 'Restarting',
      removing: 'Removing',
      dead: 'Dead',
    }[state] || state
  );
}

/** A stable color for a service name in the log viewer. */
const palette = ['#60a5fa', '#34d399', '#fbbf24', '#f472b6', '#a78bfa', '#22d3ee', '#fb923c', '#a3e635'];
export function nameColor(name) {
  let h = 2166136261; // FNV-1a
  for (const c of name || '') h = Math.imul(h ^ c.charCodeAt(0), 16777619) >>> 0;
  return palette[h % palette.length];
}

// The host app links point at: the address set in Settings (for when
// Homestack is reached on a management address the apps don't listen on),
// else the address this browser reached Homestack on, so links work on the
// LAN and over WireGuard alike.
function linkHost() {
  const h = session.appHost || location.hostname;
  return h.includes(':') && !h.startsWith('[') ? `[${h}]` : h;
}

/** Link for a published port. */
export function portUrl(port) {
  const proto = port.private === 443 || port.public === 443 ? 'https' : 'http';
  return `${proto}://${linkHost()}:${port.public}`;
}

// App links may use [HOST] for the host above.
export function appUrl(url) {
  return (url || '').replaceAll('[HOST]', linkHost());
}

// "http://[HOST]:8097/" -> ":8097", "https://jellyfin.lan/" -> "jellyfin.lan"
export function appUrlHint(url) {
  try {
    const u = new URL(appUrl(url));
    return (url.includes('[HOST]') ? '' : u.hostname) + (u.port ? `:${u.port}` : '') + (u.pathname !== '/' ? u.pathname : '');
  } catch {
    return url;
  }
}

// Where this version's source code is (Homestack is AGPL): its release tag
// when it's a release, else the repository.
export function sourceUrl(source, version) {
  if (!source) return '';
  const base = source.replace(/\/+$/, '');
  return /^\d+\.\d+\.\d+$/.test(version || '') ? `${base}/tree/v${version}` : base;
}
