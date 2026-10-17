# Homestack

A lightweight web UI for managing Docker Compose stacks in a home lab, inspired by Portainer but focused on what a single-server home lab actually uses.

- **One ~8 MB binary** with the web UI built in. It idles at roughly 10 to 20 MB of RAM.
- **Go backend, Svelte frontend.** Everything is served locally, with no internet or CDN needed.
- **Your compose files stay plain files** in a folder (`/opt/stacks/<name>/compose.yaml`). Homestack never hides them in a database, so you can still use `docker compose` by hand.

## Features

| Area | What you can do |
| --- | --- |
| First run | System check (Docker, Compose, stacks folder, container mounts) with copy-paste fixes, then create your admin account, or restore a backup instead |
| Dashboard | Shortcuts to your apps' web pages (found automatically from ports, known apps and Unraid WebUI labels, or set by hand), with each app's own icon and a running dot; CPU (with temperature), memory (programs, ZFS cache, page cache), network and disk activity with 15-minute sparklines; array, cache, parity, mover, disk temperatures and UPS at a glance; the busiest containers; system details; counts of everything; image updates waiting |
| Stacks | List or grid view; deploy, start, stop, restart, update (pull and recreate), pull, take down, delete; live CPU and memory per service; merged live logs; compose and `.env` editor with validation; one-click links to published ports |
| App templates | 53 apps in 14 categories, like Unraid's Community Applications: media servers, the *arr apps, download clients, Immich, home automation (Home Assistant, Zigbee2MQTT, Mosquitto…), VPNs and tunnels, monitoring, productivity, backup, AI and games. Each opens a settings form built from its .env (ports, folders, generated passwords), already filled in for your server (your time zone, the address you open apps on, your shares' folder and owner), with port-clash warnings, and the compose file stays editable |
| Containers | Every container, including ones outside stacks, optionally grouped into **folders** (like Unraid's FolderView: drag and drop, name patterns, start/stop a whole folder): start, stop, restart, pause, kill, remove; live CPU and memory; logs; **in-browser console**; details, mounts, env and networks; raw inspect |
| Images | Pull, remove, clean up dangling or all unused, see what uses each image, **update alerts** |
| Volumes | List, create, remove, prune, and see which containers use each |
| Networks | **Server:** every port, bond, bridge and VLAN as a tree, with link state, speed, the active bond port, addresses, DNS and live traffic; routing and policy routing explained; edit addresses, gateway, DNS and MTU in a form, or the systemd-networkd files directly; backups to restore. Every change is reviewed as a diff, tested (each link must still reach its gateway, or it's undone at once) and **undone automatically unless you keep it**; the link you're connected through can't be taken down. **VMs:** add or remove VLAN bridges for VMs, see which VMs use each bridge, and create, start, stop and delete libvirt NAT or isolated networks (with the addresses VMs got). **Docker:** private networks, and LAN networks (macvlan, ipvlan) that give containers their own address, with the subnet, gateway and a safe address range filled in for you |
| Virtual machines | KVM VMs through libvirt: start, shut down, force off, reboot, pause; **live CPU, memory, network and disk charts** (15 minutes), the VM's **IP addresses** (from the QEMU guest agent, libvirt's DHCP leases or the ARP table), and with the guest agent its OS, hostname and disk space; **VNC console in the browser**; a **serial console tab** (a text console that keeps working when the screen freezes; one session at a time, with take-over); create VMs from an ISO (Linux or Windows, UEFI or BIOS, optional TPM, USB devices), or **from a template in seconds**, like Proxmox: download an official cloud image (Debian, Ubuntu, AlmaLinux, Rocky; checked against the publisher's checksums) or copy a prepared VM, then clone it as a linked clone (instant, thin) or a full copy, with **cloud-init** setting the hostname, login, SSH keys, fixed or DHCP address and updates on first boot; **change CPUs, memory, disks and CDs, network, display, USB passthrough and boot order in a settings form**; edit the full XML for everything else; start with the server; delete with or without disks. Hidden when libvirt isn't installed |
| Notifications | Discord alerts (with [ntfy](https://ntfy.sh) phone push as a backup when Discord fails, or for warnings and errors too) for crashed or crash-looping containers, out-of-memory kills, failing health checks, failed deploys, new image updates, VMs stopping or crashing, and Homestack restarting. Each type can be switched on or off, and repeats are grouped. `homestack notify` lets the server's own scripts post to the same channel. On Debian, [`deploy/debian/notify/`](deploy/debian/notify/README.md) adds ZFS, SMART, disk temperature, disk space, mover and SnapRAID alerts |
| Storage (Debian host agent) | **Array devices**, like Unraid's Main tab: which disk is parity, Disk 1–N and cache, with a dropdown on the empty slot to add a data or parity disk and Replace to move parity to a bigger disk, all with the array up (checked first, confirmed by serial, fstab and snapraid.conf backed up and verified, undone on Cancel); every disk in one table with its usage, live reads and writes, temperature, SMART health and scrub; **spin-down** of idle hard disks after a delay you pick; how much each parity check reads and the sync safety limits, in Settings → Storage; a page per disk for its SMART attributes, self-test log and identity (like Unraid's; a sleeping disk stays asleep unless you wake it); cache, SnapRAID status; a **ZFS cache (ARC) limit** in **Settings → Storage** (ZFS may otherwise take all but 1 GB of RAM: set now and for every boot, with a suggestion that leaves room for VMs); scrub, parity sync and check (which can be **paused** and resumed, like Unraid's), mover and **TRIM** buttons with live progress and a log; clean array stop and start, reboot and shut down behind typed confirmations; refuses anything that could harm the array (a missing disk, a failing pool, a scrub or rebuild in progress, another storage task running). It expects SnapRAID with mergerfs over the data disks, each a single-disk ZFS pool or any filesystem SnapRAID takes (adding and replacing disks from the page is for ZFS ones). A log of every mover and parity run, scheduled or not, kept for the last 30 of each. Set up with [`deploy/debian/agent/install.sh`](deploy/debian/agent/install.sh) |
| Shares | Samba shares managed like Unraid's: export (shown, hidden, off), security (Public, Secure, Private) and per-user No access / Read-only / Read-write; Samba users (add, change password, remove); who's connected. Existing shares in smb.conf are imported once, with a backup |
| Files | A basic file browser over the shares, cache, each disk and unassigned disks: shows which disk holds each item, download, **edit text files** (configs, YAML, scripts, notes; syntax highlighting, conflict detection), new file or folder, rename, delete (folders need their name typed), Unraid-style New Permissions (nobody:users, 777/666), and **copy and move with a right-click menu** (Copy, Cut, Paste; select several with Ctrl or Shift; keyboard shortcuts). Copies and moves run on the server as a storage job with progress and Stop, never alongside the mover or a parity job; they keep owners, permissions and times, show what's already there before starting, and won't mix Shares with a single disk (two views of the same files) |
| Schedules | Switch the server's checks, mover, parity sync, parity check and backup on or off and set when they run (every N minutes, hourly, daily, weekly or monthly), with the next run shown. Parity jobs stay locked until SnapRAID has done its first sync |
| Backups | One encrypted file (AES-256-GCM, passphrase) with Homestack's settings and everything it set up on the server: the array's mounts and snapraid.conf, ZFS pools by ID, Docker's settings, shares and their users (with their user IDs and passwords), schedules and alerts, fan curves, the UPS, VM definitions and the network. Nightly to a folder off the boot drive, keeping the newest 14, plus download. Restore picks parts, keeps every file it replaces, and only adds the array's mounts to fstab. If the boot drive dies, follow [RECOVERY.md](RECOVERY.md) |
| Fans | Per-header control of the motherboard's PWM fans: BIOS, fixed speed, or a curve that follows the hottest of any temperatures you pick (CPU, board, NVMe, individual disks or groups), without waking sleeping disks. Full speed if a temperature goes missing, alerts when a fan stops, and every header goes back to the BIOS if the agent stops |
| Stacks started elsewhere | Compose projects outside your stacks folder are listed as "external", so their containers can still be managed |
| Settings | Allowed networks, listen interfaces, session length, stacks folder, the address app links use (for a separate management port), update checks, auto-prune, mover age, **where the storage is** (the shares, array, cache, disk and parity paths, snapraid.conf and the shares' owner, Unraid's paths by default, each change checked against what's mounted), account, signed-in devices, theme |

Deliberately left out: multiple users and roles, multiple hosts, Kubernetes and Swarm. A single-admin home lab doesn't need them.

How the server is put together (its disks, mounts and services, and the storage jobs done by hand when Homestack is down) is in [SERVER.md](SERVER.md).

---

## Option A: run it in Docker (recommended)

1. Put [`compose.yaml`](compose.yaml) in a folder **outside** your stacks folder, for example `/opt/homestack`, and start it. It runs the published image (amd64 and arm64):

   ```bash
   sudo mkdir -p /opt/homestack && cd /opt/homestack
   sudo curl -fsSLO https://raw.githubusercontent.com/leviathonbeast/homestack/main/compose.yaml
   echo TZ=Europe/London | sudo tee .env   # your time zone
   sudo docker compose up -d
   ```

2. Open `http://<server-ip>:5080`. The first-run screen checks everything, then asks you to create your admin account.

To update: `sudo docker compose pull && sudo docker compose up -d`. Releases are listed on the [Releases page](https://github.com/leviathonbeast/homestack/releases). To build the image yourself instead, from a clone of the repository: `docker build --pull -t ghcr.io/leviathonbeast/homestack:latest .`, then `docker compose up -d`.

The Storage, Fans and Networks → Server pages need the host agent too. It's set up from a clone of the repository: see [SERVER.md](SERVER.md).

The included `compose.yaml` does three important things:

- It mounts `/var/run/docker.sock` so Homestack can control Docker.
- It mounts `/opt/stacks:/opt/stacks` **at the same path**. Compose sends paths like `./config` to the Docker daemon, which resolves them on the host, so the paths must match. The system check warns you if they don't.
- It uses `network_mode: host` so the "Listen on" setting can pick your real LAN and WireGuard interfaces, and the allowlist sees real client IPs.

If your stacks live somewhere else, change both sides of that volume (for example `/srv/stacks:/srv/stacks`) and set the folder in **Settings → Stacks and VMs**.

## Option B: run the binary directly

You need Go 1.27.2 or newer and Node 24 or newer to build (the Dockerfile uses golang:1.27.2-alpine3.24 and node:24-alpine3.24). The server itself needs Docker with the Compose plugin.

```bash
make build          # builds the web UI, then bin/homestack
sudo make install   # installs to /usr/local/bin and starts the systemd service
```

For a Raspberry Pi or other 64-bit ARM board, run `make web arm64` and copy `bin/homestack` across.

Settings are stored in `/var/lib/homestack/homestack.json` when running as root. You can change the port and folders in `deploy/homestack.service`.

---

## Configuration

Almost everything is set in the UI. A few things have to be known before the UI starts, so they're environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HOMESTACK_PORT` | `5080` | Port to listen on |
| `HOMESTACK_DATA` | `/data` in Docker, `/var/lib/homestack` as root | Where `homestack.json` (account, sessions, settings) lives |
| `HOMESTACK_STACKS` | `/opt/stacks` | Stacks folder used on first start (changeable in the UI later) |
| `HOMESTACK_TRUSTED_PROXIES` | none | Comma-separated IPs or ranges of reverse proxies allowed to set `X-Forwarded-For` |
| `HOMESTACK_SECURE_COOKIE` | `false` | Set to `true` when serving over HTTPS through a reverse proxy |
| `HOMESTACK_LIBVIRT_URI` | `qemu:///system` | libvirt connection for the VMs page |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Also accepts `tcp://host:port`, e.g. a docker-socket-proxy |

## Security model

Access to the Docker socket is effectively root on the host, so Homestack is built to stay on your LAN or VPN:

- **Network allowlist.** Requests from outside your allowed networks are refused before the sign-in page. The default allows only private ranges (LAN, `10.x`, `172.16.x`, `192.168.x`, the `100.64.x` range that mesh VPNs use, and loopback).
- **Listen on specific interfaces.** You can bind to only your LAN and WireGuard addresses. If an interface isn't up yet at boot, it's retried every 10 seconds.
- **Lockout protection.** You can't save a change that excludes the address you're connected from. Every network change must be confirmed within 60 seconds, or it's rolled back automatically.
- **One admin account.** Passwords are hashed with PBKDF2-SHA256 (600,000 iterations, Go standard library). Five wrong attempts lock that IP out for 5 minutes.
- **Sessions.** Cookies are `HttpOnly` and `SameSite=Strict`, and only a hash of each token is stored. Changing your password signs out every other device. You can also sign out individual devices in Settings.
- **CSRF and headers.** Every state-changing request needs a custom header. WebSockets check the request origin. Responses carry a strict Content Security Policy.
- **Few outbound calls.** The only outside contact is update checks, which Docker makes to your registries on Homestack's behalf, and Discord notifications if you set them up. Both can be turned off. Adding a VM template downloads the image and its checksum list from the address shown, over HTTPS only, and only when you ask. Notifications only post to a Discord webhook: nothing listens for commands, messages never @mention anyone, and the webhook URL is never sent back to the browser.

- **Server network changes.** Every change to the host's own network is shown as a diff first, needs the interface name typed when it touches the link you're connected through, is tested against each link's gateway (and undone straight away if one stops answering), and is undone after 90 seconds unless you keep it from the page. The host agent owns the timer, so the undo happens even if Homestack can't be reached, and on an agent restart. A backup is saved before every change, and the original is kept.
- **Copy and move.** Every copy or move is checked first: not into itself, not between Shares and a single disk or the cache (they show the same files, so a file could overwrite itself), not SnapRAID's files or whole disks, and only with enough free space. Files are written under a temporary name and renamed into place, and a move deletes each original only once its copy is complete and saved to disk, so stopping halfway never loses or half-writes a file. The agent works inside /mnt through a confined directory handle, so a folder swapped for a symlink mid-copy can't lead it elsewhere.
- **VM templates.** A download is deleted unless it matches a SHA-256 or SHA-512 checksum (the publisher's list, or one you enter), and must be a plain qcow2 or raw disk; images that point to another file (a qcow2 backing file) are refused. A template's image is never attached to a VM as a writable disk, and can't be deleted while linked clones read from it. VM passwords are stored in the VM only as a SHA-512 crypt hash, never by Homestack; the SSH public keys you use are remembered to fill in next time.
- **Host agent.** The Storage, Fans and Networks → Server pages talk to a separate root service on the host through a unix socket (`/run/homestack/agent.sock`), not the network. It only runs a fixed list of actions, each checked before it starts, and never runs commands from the browser. Network changes only write checked `.network`, `.netdev` and `.link` files in `/etc/systemd/network`.

Don't expose Homestack to the internet. Reach it over your LAN or WireGuard.

### If you get locked out

Run these on the server. A running Homestack picks up the change within a few seconds, with no restart needed.

```bash
# Allow every private network and all interfaces again
homestack reset-access                          # binary install (as root)
docker exec homestack homestack reset-access    # Docker install

# Set a new random admin password (printed once) and sign out everywhere
homestack reset-password
docker exec homestack homestack reset-password
```

---

## How it works

```
Browser ──HTTP/SSE/WebSocket──▶ Homestack (Go)
                                   ├── Docker Engine API over /var/run/docker.sock
                                   │     containers, logs, stats, exec, images, volumes, networks, events
                                   ├── `docker compose` CLI, run in each stack folder
                                   │     up, down, stop, restart, pull, config validation
                                   └── homestack.json (account, sessions, settings)
```

- **Stacks are folders.** Containers are matched to stacks using the labels Compose adds (`com.docker.compose.project.working_dir`), so stacks you deployed by hand before installing Homestack show up automatically.
- **Live updates.** A Docker events stream keeps every page current without polling. Logs and stats use Server-Sent Events, and the console uses a WebSocket.
- **Update checks.** Homestack asks the Docker daemon for each image's current registry digest (`GET /distribution/<image>/json`) and compares it with the local one. Only the small manifest is fetched, never the image. Nothing is updated until you click **Update**.
- **Delete is recoverable.** Deleting a stack takes it down and moves its folder to `<stacks>/.trash/<name>-<date>`. Empty that folder yourself when you're sure.

## Development

```bash
# Terminal 1: the API (uses ./data for settings)
HOMESTACK_DATA=./data go run .

# Terminal 2: the UI with hot reload on http://localhost:5173 (proxies /api to :5080)
cd web && npm install && npm run dev

go test ./...
```

### Checks

Turn on the git hooks once per clone, so the checks run without having to remember them:

```bash
make hooks   # or: git config core.hooksPath .githooks
```

- **Before every commit**, `scripts/check.sh --quick` checks what's staged, offline: LF line endings, gofmt, go vet, staticcheck, svelte-check and ShellCheck.
- **Before every push**, `scripts/check.sh` (or `make check`) runs all of that plus the tests, `govulncheck`, `npm audit`, a UI build, and checks that every dependency is on its latest stable release: Go modules, npm packages, Go, Node LTS, npm and Alpine. On Windows the tests run in Docker, since the code is Linux-only.
- **Every image build** runs `go vet` and the tests, so broken code can't become an image, and `--pull` builds on the latest base images.

A check that can't run counts as a failure. `--no-verify` skips a hook once, for emergencies only. The checks need Go, Node.js, ShellCheck (`winget install koalaman.shellcheck` or `apt install shellcheck`), and on Windows, Docker Desktop.

### Project layout

```
main.go                  commands: serve, reset-access, reset-password, healthcheck, notify
internal/state/          homestack.json store and password hashing
internal/docker/         Docker API client (socket), compose runner, logs, stats, exec
internal/server/         HTTP API: auth, access control, stacks, containers, resources,
                         live streams, update checker, system checks
internal/templates/      app templates (the catalogue on the New stack page)
internal/sysinfo/        host CPU, temperature, memory, network, disk I/O and space, with history
internal/libvirt/        KVM virtual machines through virsh (list, power, create, settings form, console, templates)
internal/cloudinit/      cloud-init seed disks for new VMs (written as ISO 9660 in Go), password hashing
internal/cloudimg/       the cloud image catalogue and checksum-verified downloads
internal/notify/         Discord and ntfy notifications (queue, rate limits, de-duplication)
internal/agent/          the host service (`homestack agent`): storage, parity, mover, fans,
                         schedules, shares, files, disk replacement, run logs
internal/fans/           fan curve settings shared by the agent and the UI
web/src/                 Svelte 5 app (pages/, components/, lib/)
deploy/homestack.service systemd unit
deploy/debian/           host setup for a Debian server: the agent, alert and job scripts,
                         networking, updates, UPS
```

## Licence

Homestack is free software under the [GNU Affero General Public License](LICENSE), version 3 or (at your option) any later version (`AGPL-3.0-or-later`). You can use it for anything, change it and share it. If you share a changed version, or let other people use one over a network, you must offer them its source code under the same licence.

Copyright © 2026 leviathonbeast. The libraries it's built with keep their own licences: MIT (the web UI's libraries), ISC (coder/websocket), BSD (Go's standard library) and MPL-2.0 (noVNC).
