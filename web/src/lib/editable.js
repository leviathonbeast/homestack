// Which files the file browser opens in the editor, and with what
// highlighting (see CodeEditor's languages). Anything else can still be
// opened as text with the edit button; the agent refuses binary files.

const byExt = {
  yaml: 'yaml', yml: 'yaml',
  xml: 'xml', svg: 'xml', plist: 'xml', xsd: 'xml', xsl: 'xml', nfo: 'xml', opml: 'xml',
  html: 'html', htm: 'html',
  json: 'json', jsonc: 'json', json5: 'json', webmanifest: 'json',
  js: 'javascript', mjs: 'javascript', cjs: 'javascript', ts: 'typescript',
  py: 'python', pyw: 'python',
  sh: 'shell', bash: 'shell', zsh: 'shell', ash: 'shell', bashrc: 'shell', profile: 'shell',
  conf: 'ini', cfg: 'ini', ini: 'ini', cnf: 'ini', properties: 'ini', desktop: 'ini',
  service: 'ini', timer: 'ini', mount: 'ini', socket: 'ini', network: 'ini', netdev: 'ini', link: 'ini',
  env: 'env', toml: 'toml',
  css: 'css', scss: 'css', less: 'css',
  sql: 'sql', lua: 'lua', go: 'go', rs: 'rust', rb: 'ruby', pl: 'perl', pm: 'perl',
  diff: 'diff', patch: 'diff', ps1: 'powershell', psm1: 'powershell', j2: 'jinja2', jinja: 'jinja2', jinja2: 'jinja2',
  txt: 'text', text: 'text', log: 'text', md: 'text', markdown: 'text', rst: 'text', csv: 'text', tsv: 'text',
  srt: 'text', vtt: 'text', ass: 'text', ssa: 'text', sub: 'text', cue: 'text', m3u: 'text', m3u8: 'text', pls: 'text',
  rules: 'text', list: 'text', lst: 'text', gitignore: 'text', dockerignore: 'text', gitattributes: 'text',
  nft: 'text', pem: 'text', crt: 'text', pub: 'text', ovpn: 'text', bak: 'text', old: 'text', orig: 'text',
};

const byName = {
  dockerfile: 'dockerfile', containerfile: 'dockerfile', makefile: 'text', readme: 'text', license: 'text',
  changelog: 'text', authors: 'text', crontab: 'shell', hosts: 'text', fstab: 'text', exports: 'text',
  'nginx.conf': 'nginx', 'smb.conf': 'ini', 'snapraid.conf': 'text', caddyfile: 'text', vagrantfile: 'ruby',
};

/** The editor language for a file name, or null if it isn't a known text type. */
export function editorLanguage(name) {
  const n = (name || '').toLowerCase();
  if (byName[n]) return byName[n];
  if (n.startsWith('.env') || n.endsWith('.env')) return 'env';
  if (n.startsWith('dockerfile') || n.endsWith('.dockerfile')) return 'dockerfile';
  if (/^(sites-(available|enabled)|nginx)/.test(n) || n.endsWith('.nginx')) return 'nginx';
  const dot = n.lastIndexOf('.');
  const ext = dot >= 0 ? n.slice(dot + 1) : '';
  return byExt[ext] || null;
}

/** Files up to this size can be edited (the agent's limit). */
export const maxEditBytes = 2 << 20;
