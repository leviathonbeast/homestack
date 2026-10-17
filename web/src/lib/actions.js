// Shared actions used from several pages, with their confirmations.
import { api } from './api.js';
import { runJob, confirm, toast, bumpLive } from './state.svelte.js';
import { navigate } from './router.svelte.js';

const stackVerb = {
  up: 'Start',
  stop: 'Stop',
  restart: 'Restart',
  pull: 'Pull images for',
  update: 'Update',
  down: 'Take down',
};

export async function stackAction(name, action) {
  if (action === 'down') {
    const ok = await confirm({
      title: `Take down ${name}?`,
      message: 'This stops and removes the containers and networks. Your compose file, volumes and data folders are kept, so you can start it again.',
      confirmLabel: 'Take down',
      danger: true,
    });
    if (!ok) return;
  }
  if (action === 'stop') {
    const ok = await confirm({ title: `Stop ${name}?`, message: 'Its containers stop until you start the stack again.', confirmLabel: 'Stop' });
    if (!ok) return;
  }
  return runJob(`${stackVerb[action] || action} ${name}`, 'POST', `/stacks/${encodeURIComponent(name)}/action/${action}`);
}

export async function deleteStack(name) {
  const res = await confirm({
    title: `Delete ${name}?`,
    message: 'This takes the stack down and moves its folder into .trash inside your stacks folder, so nothing in ./config or ./data is lost by accident.',
    confirmLabel: 'Delete stack',
    danger: true,
    typeToConfirm: name,
    options: [{ key: 'volumes', label: 'Also delete its named volumes', hint: 'Permanently removes data stored in Docker volumes.' }],
  });
  if (!res) return;
  const q = res.volumes ? '?volumes=1' : '';
  const r = await runJob(`Delete ${name}`, 'DELETE', `/stacks/${encodeURIComponent(name)}${q}`);
  if (r.ok) navigate('/stacks');
}

const containerVerb = { start: 'Started', stop: 'Stopped', restart: 'Restarted', kill: 'Killed', pause: 'Paused', unpause: 'Resumed' };

export async function containerAction(c, action) {
  if (action === 'kill') {
    const ok = await confirm({ title: `Kill ${c.name}?`, message: "This stops it immediately without letting it shut down cleanly.", confirmLabel: 'Kill', danger: true });
    if (!ok) return false;
  }
  try {
    await api.post(`/containers/${c.id}/action/${action}`);
    toast(`${containerVerb[action]} ${c.name}`, 'success', 2500);
    bumpLive();
    return true;
  } catch (e) {
    toast(e.message, 'error', 7000);
    return false;
  }
}

export async function removeContainer(c) {
  const res = await confirm({
    title: `Remove ${c.name}?`,
    message: c.stack
      ? `It belongs to the ${c.stack} stack, which will recreate it the next time the stack starts.`
      : "The container is deleted. Its image and named volumes are kept.",
    confirmLabel: 'Remove',
    danger: true,
    options: [
      { key: 'force', label: 'Stop it first if it’s running', checked: c.state === 'running' },
      { key: 'volumes', label: 'Also remove its anonymous volumes' },
    ],
  });
  if (!res) return false;
  const q = new URLSearchParams();
  if (res.force) q.set('force', '1');
  if (res.volumes) q.set('volumes', '1');
  try {
    await api.del(`/containers/${c.id}?${q}`);
    toast(`Removed ${c.name}`, 'success', 2500);
    bumpLive();
    return true;
  } catch (e) {
    toast(e.message, 'error', 7000);
    return false;
  }
}
