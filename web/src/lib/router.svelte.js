// A tiny client-side router using the History API.

export const route = $state({ path: location.pathname, search: location.search });

export function navigate(to, { replace = false } = {}) {
  if (to === location.pathname + location.search) return;
  if (replace) history.replaceState({}, '', to);
  else history.pushState({}, '', to);
  route.path = location.pathname;
  route.search = location.search;
  window.scrollTo(0, 0);
}

window.addEventListener('popstate', () => {
  route.path = location.pathname;
  route.search = location.search;
});

// Let plain <a href="/..."> links navigate without a page reload.
document.addEventListener('click', (e) => {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest?.('a');
  if (!a || a.target || a.hasAttribute('download')) return;
  const href = a.getAttribute('href');
  if (!href || !href.startsWith('/') || href.startsWith('//') || href.startsWith('/api/')) return;
  e.preventDefault();
  navigate(href);
});

/** Matches a path against a pattern like "/stacks/:name/:tab?". */
export function match(pattern, path) {
  const p = pattern.split('/').filter(Boolean);
  const s = path.split('/').filter(Boolean);
  const params = {};
  for (let i = 0; i < p.length; i++) {
    const seg = p[i];
    const optional = seg.endsWith('?');
    if (seg.startsWith(':')) {
      const name = seg.slice(1).replace('?', '');
      if (s[i] === undefined) {
        if (optional) continue;
        return null;
      }
      params[name] = decodeURIComponent(s[i]);
    } else if (seg !== s[i]) {
      return null;
    }
  }
  if (s.length > p.length) return null;
  return params;
}

export function query(name) {
  return new URLSearchParams(route.search).get(name);
}
