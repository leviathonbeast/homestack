# Automatic security updates

```bash
sudo bash deploy/debian/updates/install.sh
```

| File | Installed as | What it does |
| --- | --- | --- |
| `20auto-upgrades` | `/etc/apt/apt.conf.d/` | Turns on the daily update (the same as answering Yes in `dpkg-reconfigure unattended-upgrades`) |
| `52homestack-updates` | `/etc/apt/apt.conf.d/` | Security updates only; never `linux-*`, ZFS, Docker or containerd; no automatic reboot |
| `needrestart-homestack.conf` | `/etc/needrestart/conf.d/homestack.conf` | Restarts services that use a patched library, except Docker, containerd, libvirt, ZFS and the Homestack agent |
| `updates-notify` | `/usr/local/sbin/`, run after `apt-daily-upgrade.service` | Discord: what was installed, failures, and "reboot needed" (then a reminder every 7 days) |

Check it: `sudo unattended-upgrade --dry-run --debug 2>&1 | grep -E "Allowed origins|blacklist"`. Logs: `/var/log/unattended-upgrades/`. The kernel and ZFS come from trixie-backports and are never updated automatically: run `apt list --upgradable` every few weeks.
