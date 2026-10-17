# When the boot drive dies

Your files are safe: they're on the data disks, the cache and parity, and a new boot drive doesn't touch any of them. What's lost is the server's own setup: how the array is mounted, SnapRAID's config, the shares and their users, the schedules, fan curves, VM definitions, Docker's settings. A Homestack backup holds all of that, so the job is: install Debian, get Homestack running again, and restore the backup.

This takes about an hour, most of it installing packages.

The steps use Homestack's default paths (`/mnt/cache`, `/mnt/user`…). If you changed them in **Settings → Storage → Where the storage is**, use yours; the restore puts that setting back too.

## Before it happens

In Homestack, **Settings → Backups**:

1. **Set a backup passphrase**, and save it in your password manager. A restore needs it, and Homestack can't show it to you.
2. **Turn on the nightly backup.** Backups go to a folder in the shares (`/mnt/user/backups/homestack` by default: on the cache or the array, never the boot drive), and the newest 14 are kept.
3. **Now and then, Back up and download**, and keep the file off the server too.
4. **Write down what the steps below ask for**, somewhere off the server: the server's name, the first user's name, the cache disk's model and filesystem UUID (`lsblk -o NAME,MODEL,UUID,FSTYPE`), the SnapRAID version (`snapraid --version`), and where Homestack's source comes from (`git -C <source folder> remote get-url origin`).

A backup is settings only. Your files, your apps' data (`/mnt/user/appdata`) and VM disks stay on the array and the cache. Docker's own folder (images, named volumes, and any `docker login`) is on the boot drive: the images download again, but keep apps' data in folders, not named volumes.

## Bringing the server back

You need: the new boot drive, a Debian 13 USB stick, and the backup passphrase.

### 1. Install Debian 13 on the new drive

- **Unplug the data disks, the parity disks and any unassigned disks first**, so the installer can't touch them. Leave the cache in, but never select it in the installer (check the model against your note).
- Hostname: the same as before.
- **First user: the same name as before**, so it gets user ID 1000 again, the ID your files belong to.
- Software: just **SSH server** and **standard system utilities**. No desktop.

When it's installed, shut down, plug the drives back in, and start it.

When it's up, log in and update it:

```bash
sudo apt update && sudo apt full-upgrade -y
```

### 2. Install ZFS

ZFS 2.4 comes from backports (Debian's standard 2.3 is older than your pools). Stop Debian importing pools by itself **before** installing ZFS: a disk that was once in a pool can keep an old label with the same name, and a scan could pick the wrong one. The restore imports each pool by its GUID.

```bash
sudo sed -i 's/^Components: .*/Components: main contrib non-free non-free-firmware/' /etc/apt/sources.list.d/debian.sources
sudo tee /etc/apt/sources.list.d/backports.sources >/dev/null <<'SRC'
Types: deb
URIs: http://deb.debian.org/debian
Suites: trixie-backports
Components: main contrib
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
SRC
sudo systemctl mask zfs-import-scan.service zfs-import-cache.service zfs-mount.service
sudo apt update
sudo apt install -y -t trixie-backports linux-image-amd64 linux-headers-amd64
sudo reboot
```

After the reboot:

```bash
sudo apt install -y -t trixie-backports zfs-dkms zfsutils-linux
zfs version           # 2.4.x
zpool list            # "no pools available" is right: the restore imports them
sudo systemctl unmask zfs-import-cache.service zfs-mount.service
sudo systemctl enable zfs-import-cache.service zfs-mount.service zfs.target
```

`zfs-import-scan` stays masked for good.

### 3. Install everything else

```bash
sudo apt install -y mergerfs btrfs-progs xfsprogs samba smartmontools rsync parted \
  qemu-system-x86 qemu-system-modules-spice libvirt-daemon-system virtinst ovmf virtiofsd lm-sensors
```

SnapRAID: **the same version as before** (from your note), not Debian's own package, which is older. An older SnapRAID may not read the content file a newer one wrote, or newer options in `snapraid.conf` (like 15.0's `snapshot`). Each release has a `.deb`:

```bash
VER=<the version you noted, like 15.0>
curl -fLO "https://github.com/amadvance/snapraid/releases/download/v$VER/snapraid_${VER}-1_amd64.deb"
sudo apt install -y "./snapraid_${VER}-1_amd64.deb"
snapraid --version
```

Then Docker, from Docker's own repository: follow [docs.docker.com/engine/install/debian](https://docs.docker.com/engine/install/debian/), including `docker-compose-plugin`.

If your shares are owned by user ID 99 (Unraid's `nobody`, Homestack's default owner), make an account for it; the restore brings back normal users (1000 and up), not system ones:

```bash
sudo useradd --system --uid 99 --gid 100 --no-create-home --shell /usr/sbin/nologin unraid
```

### 4. Mount the cache by hand

Homestack, its settings and your backups are on the cache. Mount it, just for now (the restore adds it to `/etc/fstab` properly):

```bash
lsblk -o NAME,MODEL,SIZE,FSTYPE,UUID     # find the cache: its model, and the UUID you noted
sudo mkdir -p /mnt/cache
sudo mount UUID=<the cache's UUID> /mnt/cache
ls /mnt/cache/appdata/homestack-src /mnt/cache/appdata/homestack /mnt/cache/backups/homestack
```

If the cache was replaced or died too, see [If the cache is gone too](#if-the-cache-is-gone-too).

### 5. Start Homestack

Straight from the cache for now. Step 9 switches it to the usual paths once `/mnt/user` is back.

```bash
sudo mkdir -p /run/homestack
sudo docker run -d --name homestack --restart unless-stopped --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /run/homestack:/run/homestack \
  -v /mnt/cache/appdata/homestack:/data \
  ghcr.io/leviathonbeast/homestack:latest
```

Open `http://<the server's address>:5080` and sign in with your usual account: Homestack's settings were on the cache, so they're all there.

### 6. Install the host agent

```bash
sudo bash /mnt/cache/appdata/homestack-src/deploy/debian/agent/install.sh
```

### 7. Restore the backup

In Homestack, **Settings → Backups**. The saved backups show up even though the array isn't mounted yet: Homestack finds them on the cache and the data disks.

1. Pick the newest, **Restore**, and type the passphrase.
2. Leave the ticks as they are. **Network** is off to begin with: tick it if this is the same machine with the same network cards (it is, if only the boot drive died).
3. Type RESTORE.

What it does, in order:

- **The array**: where the storage is (Settings → Storage), then imports the ZFS pools (by their IDs, never an old label with the same name), adds the storage's mounts to `/etc/fstab` and mounts them. `/mnt/user` is back. SnapRAID's config and the ZFS cache limit come back too, and it warns if this server's SnapRAID is older than the backup's.
- **System settings**: Docker's settings, and Docker and the VMs waiting for the array at boot.
- **Shares**: Samba's settings, the share users with their old user IDs (so the files on the array still belong to them), and their passwords.
- **Schedules and alerts, fan curves, the UPS, VM definitions**, and the network files if you ticked them.

Every file it replaces is kept next to it (`….homestack-restore-<time>`). If a part says it couldn't do something, it says why; fix that and restore just that part again.

No backups listed? Restore one you downloaded instead: **Restore from a file**.

### 8. Reinstall the helper scripts

The restored schedules run scripts these install. They keep the settings the restore brought back.

```bash
cd /mnt/user/appdata/homestack-src/deploy/debian
sudo bash notify/install.sh
sudo bash ups/install.sh        # if you have the UPS
sudo bash updates/install.sh
```

### 9. Restart

```bash
sudo docker rm -f homestack
sudo docker run -d --name homestack --restart unless-stopped --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /run/homestack:/run/homestack \
  -v /mnt/user/appdata/homestack:/data \
  -v /mnt/user/appdata/stacks:/mnt/user/appdata/stacks \
  -v /var/run/libvirt:/var/run/libvirt \
  -v /mnt/user/isos:/mnt/user/isos:ro \
  ghcr.io/leviathonbeast/homestack:latest
sudo reboot
```

The reboot proves everything comes up by itself: the pools, the cache and `/mnt/user`, the network if you restored it, then Docker and the VMs.

### 10. Check, then start things

- **Storage**: every disk *Working*, parity *Up to date*. Parity didn't change: the array was never touched. If in doubt, `sudo snapraid diff` lists anything that did.
- **Stacks**: start each one. Docker downloads the images again. If some come from a registry you run yourself (a Gitea, say), start that stack first and `docker login` to it.
- **VMs**: start them.
- **Settings → Backups**: the passphrase is set again (from the backup's) and the nightly backup is on. Make one now.

## If the cache is gone too

Then Homestack's settings, its copy of the source and the newest backups went with it. You need a backup you downloaded, and the source from your git server.

1. Do steps 1 to 3 above.
2. Get the source and start a fresh Homestack:

   ```bash
   sudo git clone <where the source comes from, from your note> /opt/homestack-src
   sudo mkdir -p /run/homestack /opt/homestack-data
   sudo docker run -d --name homestack --restart unless-stopped --network host \
     -v /var/run/docker.sock:/var/run/docker.sock -v /run/homestack:/run/homestack \
     -v /opt/homestack-data:/data ghcr.io/leviathonbeast/homestack:latest
   ```

3. Open Homestack. On the first screen, choose **Restore from a backup**, pick the file and type the passphrase. Your account and settings come back; sign in with them.
4. Install the host agent: `sudo bash /opt/homestack-src/deploy/debian/agent/install.sh`.
5. **Settings → Backups → Restore from a file**, the same file again, now for the server's parts (step 7 above).
6. Set up the new cache disk, then steps 8 to 10, with `/opt/homestack-src` in place of `/mnt/user/appdata/homestack-src`.

Your apps' data (`/mnt/user/appdata`) lived on the cache, and a Homestack backup doesn't hold it, so the apps start empty unless you kept a copy of it elsewhere.
