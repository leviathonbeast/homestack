// Small wrapper around fetch for the Homestack API.
import { session } from './state.svelte.js';

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

async function request(method, path, body) {
  const headers = { 'X-Homestack': '1' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res;
  try {
    res = await fetch('/api' + path, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      credentials: 'same-origin',
    });
  } catch {
    throw new ApiError("Can't reach Homestack. Check your connection.", 0);
  }
  if (res.status === 401 && !path.startsWith('/login') && !path.startsWith('/setup')) {
    session.authenticated = false;
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(data.error || res.statusText || 'Something went wrong.', res.status);
  return data;
}

export const api = {
  get: (p) => request('GET', p),
  post: (p, b = {}) => request('POST', p, b),
  put: (p, b = {}) => request('PUT', p, b),
  del: (p) => request('DELETE', p),
};

/**
 * Runs a streaming action (compose commands, image pulls). Each output line
 * is passed to onLine; resolves with { ok, error } when it finishes.
 */
export async function stream(method, path, body, onLine) {
  let res;
  try {
    res = await fetch('/api' + path, {
      method,
      headers: { 'X-Homestack': '1', 'Content-Type': 'application/json' },
      body: body !== undefined ? JSON.stringify(body) : undefined,
      credentials: 'same-origin',
    });
  } catch {
    return { ok: false, error: "Can't reach Homestack. Check your connection." };
  }
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    if (res.status === 401) session.authenticated = false;
    return { ok: false, error: data.error || res.statusText };
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let result = { ok: false, error: 'The connection closed before the action finished. It may still be running.' };
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      if (!line.trim()) continue;
      try {
        const msg = JSON.parse(line);
        if (msg.type === 'line') onLine?.(msg.text);
        else if (msg.type === 'done') result = { ok: msg.ok, error: msg.error };
      } catch {
        onLine?.(line);
      }
    }
  }
  return result;
}

/**
 * Subscribes to a Server-Sent Events stream. handlers maps event names to
 * callbacks. Returns a function that closes the stream.
 */
export function subscribe(path, handlers) {
  const es = new EventSource('/api' + path, { withCredentials: true });
  for (const [name, fn] of Object.entries(handlers)) {
    if (name === 'open' || name === 'error') continue;
    es.addEventListener(name, (e) => {
      try {
        fn(JSON.parse(e.data));
      } catch {
        /* ignore malformed */
      }
    });
  }
  if (handlers.open) es.onopen = handlers.open;
  if (handlers.error) es.onerror = handlers.error;
  return () => es.close();
}
