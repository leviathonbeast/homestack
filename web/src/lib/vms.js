// Shared VM actions and labels.
import { api } from './api.js';
import { confirm, toast } from './state.svelte.js';
import { navigate } from './router.svelte.js';

export const vmStateLabel = {
  running: 'Running',
  paused: 'Paused',
  shutoff: 'Stopped',
  'shutting-down': 'Shutting down',
  crashed: 'Crashed',
  suspended: 'Suspended',
  unknown: 'Unknown',
};

export function vmStateColor(state) {
  return { running: 'green', paused: 'amber', 'shutting-down': 'amber', suspended: 'amber', crashed: 'red' }[state] || '';
}

const done = {
  start: 'Started',
  shutdown: 'Asked to shut down',
  stop: 'Forced off',
  reboot: 'Rebooting',
  reset: 'Reset',
  pause: 'Paused',
  resume: 'Resumed',
};

export async function vmAction(vm, action) {
  if (action === 'stop') {
    const ok = await confirm({
      title: `Force off ${vm.name}?`,
      message: 'This cuts the power immediately, like pulling the plug. Unsaved work inside the VM is lost. Use Shut down for a clean stop.',
      confirmLabel: 'Force off',
      danger: true,
    });
    if (!ok) return false;
  }
  if (action === 'reset') {
    const ok = await confirm({ title: `Reset ${vm.name}?`, message: 'This restarts it instantly without shutting down cleanly.', confirmLabel: 'Reset', danger: true });
    if (!ok) return false;
  }
  try {
    await api.post(`/vms/${encodeURIComponent(vm.name)}/action/${action}`);
    toast(`${done[action]}: ${vm.name}`, 'success', 2500);
    if (action === 'shutdown') toast('The guest decides when to power off. If it ignores the request, use Force off.', 'info', 5000);
    return true;
  } catch (e) {
    toast(e.message, 'error', 7000);
    return false;
  }
}

export async function setAutostart(vm, on) {
  try {
    await api.post(`/vms/${encodeURIComponent(vm.name)}/autostart`, { on });
    toast(on ? `${vm.name} will start with the server` : `${vm.name} won't start with the server`, 'success', 2500);
    return true;
  } catch (e) {
    toast(e.message, 'error');
    return false;
  }
}

export async function deleteVM(vm) {
  if (vm.state !== 'shutoff' && vm.state !== 'crashed') {
    toast('Stop the VM before deleting it.', 'warn');
    return false;
  }
  const disks = vm.disks.filter((d) => d.device === 'disk' && d.path && !d.path.startsWith('/dev/'));
  const res = await confirm({
    title: `Delete ${vm.name}?`,
    message: 'This removes the VM and its UEFI settings. Its disk files are kept unless you tick the box below.',
    confirmLabel: 'Delete VM',
    danger: true,
    typeToConfirm: vm.name,
    options: disks.length
      ? [{ key: 'disks', label: `Also delete its disk${disks.length > 1 ? 's' : ''}`, hint: disks.map((d) => d.path).join(', ') }]
      : [],
  });
  if (!res) return false;
  const wipe = typeof res === 'object' && res.disks;
  try {
    await api.del(`/vms/${encodeURIComponent(vm.name)}${wipe ? '?disks=1' : ''}`);
    toast(`Deleted ${vm.name}`, 'success');
    navigate('/vms');
    return true;
  } catch (e) {
    toast(e.message, 'error', 8000);
    return false;
  }
}

export function vmMenu(vm, { onChange } = {}) {
  const run = async (a) => (await vmAction(vm, a)) && onChange?.();
  const on = vm.state === 'running';
  const paused = vm.state === 'paused';
  return [
    on ? { label: 'Open console', icon: 'terminal', onclick: () => navigate(`/vms/${encodeURIComponent(vm.name)}/console`) } : null,
    on ? { label: 'Shut down', icon: 'power', hint: 'Ask the guest to power off', onclick: () => run('shutdown') } : null,
    on ? { label: 'Reboot', icon: 'restart', onclick: () => run('reboot') } : null,
    on ? { label: 'Pause', icon: 'pause', onclick: () => run('pause') } : null,
    paused ? { label: 'Resume', icon: 'play', onclick: () => run('resume') } : null,
    on || paused ? { label: 'Force off', icon: 'stop', hint: 'Like pulling the plug', onclick: () => run('stop') } : null,
    { divider: true },
    { label: vm.autostart ? "Don't start with server" : 'Start with server', icon: 'power', onclick: async () => (await setAutostart(vm, !vm.autostart)) && onChange?.() },
    { label: 'Edit settings', icon: 'settings', onclick: () => navigate(`/vms/${encodeURIComponent(vm.name)}/settings`) },
    { label: 'Edit XML', icon: 'code', hint: 'Advanced', onclick: () => navigate(`/vms/${encodeURIComponent(vm.name)}/xml`) },
    { label: 'Make a template', icon: 'copy', hint: 'Copy its disk, to clone new VMs from', onclick: () => navigate(`/vms/templates?from=${encodeURIComponent(vm.name)}`) },
    { divider: true },
    { label: 'Delete VM', icon: 'trash', danger: true, onclick: () => deleteVM(vm) },
  ];
}
