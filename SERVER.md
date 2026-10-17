# The server

How a Homestack server is put together, and how to do the storage jobs by hand when Homestack is down. If the boot drive dies, follow [RECOVERY.md](RECOVERY.md) instead.

The paths below are Homestack's defaults (Unraid's). **Settings → Storage → Where the storage is** shows yours. The agent also writes them to `/etc/default/homestack-layout`, so `. /etc/default/homestack-layout` gives you `$STORAGE_SHARES`, `$STORAGE_CACHE` and the rest in a shell.

## Finding your disks

Device letters (`sda`, `sdb`…) change between boots, so never go by them. Go by `/dev/disk/by-id/` (model and serial), filesystem UUIDs, and pool GUIDs:

```bash
lsblk -o NAME,MODEL,SERIAL,SIZE,FSTYPE,LABEL,MOUNTPOINTS   # every disk, with what's on it
ls -l /dev/disk/by-id/ | grep -v -- -part                  # the stable names
zpool list -o name,guid,health                             # the data pools and their GUIDs
findmnt -t zfs,btrfs,xfs,ext4,fuse.mergerfs                # what's mounted where
```

Homestack's **Storage** page shows the same: each disk's slot, model, serial and health.

**Never import a pool by name: use its GUID** (`zpool import <guid>`). A disk that was once in a pool can keep an old label, and `zpool import` lists it as a second pool with the same name. A backup's storage part records each pool's GUID.

Parity has to be at least as big as the biggest data disk. **Array devices** on the Storage page shows both, and says why a disk doesn't fit a slot.

## How it fits together

| What | Where (default) | What it is |
| --- | --- | --- |
| Data disks | `/mnt/disk1`, `/mnt/disk2`… | One ZFS pool per disk, named after its folder (`disk1`…), or any filesystem SnapRAID takes (ext4, XFS…) mounted from fstab |
| Parity | `/mnt/parity1`, `/mnt/parity2`… | One ext4 or XFS disk per parity level, holding SnapRAID's parity file |
| Cache | `/mnt/cache` | The fast disk new files land on |
| Shares | `/mnt/user` | mergerfs over the cache and the data disks: what shares and apps use |
| Array | `/mnt/user-array` | mergerfs over the data disks only: where the mover moves files to |
| Apps' data | `/mnt/user/appdata` | The cache's `appdata` folder bound straight through, so apps get the cache's own filesystem (mergerfs can't do what SQLite and some indexers need) |
| Unassigned disks | `/mnt/disks/<name>` | Other disks, each in a folder of its own |
| SnapRAID | `/etc/snapraid.conf` | Lists the data and parity disks |

- Data disks that aren't ZFS work too: Storage shows them, and stopping and starting the array unmounts and mounts them (from `/etc/fstab`). Adding and replacing a disk from Storage creates a ZFS pool, so for those it says how to do it by hand instead.
- The mover runs nightly. It moves files older than the age set in Settings → Storage from the cache to the array, for the shares that have **Move to the array nightly** switched on.
- On the array, mergerfs only puts a new file on a disk that already has that share's folder. So a new disk needs the share folders before it gets any files (Array devices makes them).
- Shares are plain folders, not datasets, because SnapRAID skips anything on a different filesystem. If a share ever turns into a dataset, `deploy/debian/flatten-dataset` turns it back.
- Shared files belong to one owner (`nobody:users`, 99:100, by default; set under Owner in Settings → Storage). New share folders, **New permissions** in Files and the apps you install use it.

## Mounts and services

**ZFS.** The pools come back at boot from `/etc/zfs/zpool.cache` (`zfs-import-cache` and `zfs-mount` enabled). `zfs-import-scan` stays masked for good. **Never run `zpool upgrade`.** Monthly scrubs run from `/etc/cron.d/zfsutils-linux`.

**Kernel updates.** Before rebooting into a new kernel, run `dkms status` and check zfs says `installed` for it. If it doesn't, don't reboot: your pools wouldn't import. The previous kernel is always under GRUB's **Advanced options**.

**`/etc/fstab`** has the storage's lines next to the boot drive's own. With the default paths, their shape is:

```fstab
UUID=<cache's UUID>  /mnt/cache  btrfs  defaults,noatime,nofail  0 0
UUID=<parity's UUID>  /mnt/parity1  ext4  defaults,nofail  0 2
/mnt/disk1:/mnt/disk2:…  /mnt/user-array  fuse.mergerfs  cache.files=off,category.create=epmfs,func.getattr=newest,dropcacheonclose=false,moveonenospc=true,minfreespace=50G,fsname=array,x-systemd.requires=zfs-mount.service,nofail  0 0
/mnt/cache:/mnt/disk1:/mnt/disk2:…  /mnt/user  fuse.mergerfs  cache.files=off,category.create=ff,func.getattr=newest,dropcacheonclose=false,moveonenospc=true,minfreespace=100G,fsname=user,x-systemd.requires=zfs-mount.service,nofail  0 0
/mnt/cache/appdata  /mnt/user/appdata  none  bind,nofail,x-systemd.requires-mounts-for=/mnt/cache,x-systemd.requires-mounts-for=/mnt/user  0 0
```

Every storage line has `nofail`, so a missing disk can't stop the server booting. `lsblk -f` gives the UUIDs. The data disks aren't in fstab: ZFS mounts them.

**Docker and the VMs wait for the storage** at every boot, so nothing starts against empty folders. Each of `docker`, `libvirtd` and `virtqemud` has `/etc/systemd/system/<name>.service.d/wait-for-storage.conf`:

```ini
[Unit]
RequiresMountsFor=/mnt/cache /mnt/user /mnt/user-array /mnt/user/appdata
After=zfs-mount.service
```

**Docker** keeps its logs small and doesn't block the VMs' bridged traffic (`/etc/docker/daemon.json`):

```json
{ "log-driver": "json-file", "log-opts": { "max-size": "50m", "max-file": "3" }, "ip-forward-no-drop": true }
```

Docker's own folder (images, and named volumes) is on the boot drive, so it's lost with it. Keep apps' data in folders under the apps' data folder instead, where it's on the cache. If your stacks pull images from your own registry, log in to it once (`docker login <registry>`); that login is on the boot drive too.

**Spin-down**: **Settings → Storage → Spin down** sets how long a SATA hard disk may sit with no reads or writes before the agent puts it in standby (`hdparm -y`); off unless you set it. SSDs, NVMe and the boot drive are never spun down, nor is anything while a storage job runs. Each disk can also be spun down from its menu on Storage, which shows every disk's reads and writes as they happen (from `/proc/diskstats`, which never wakes a disk). If a disk keeps waking, something is reading it: its Read column shows when. A paused parity job leaves the disks idle, so they can spin down while it waits.

**TRIM**: **TRIM now** on Storage's Cache card tells the SSDs which blocks are free, so they stay fast. It runs `fstrim --all` (every mounted filesystem on an SSD, including the boot drive) and then `zpool trim` on each ZFS pool (fstrim can't trim ZFS; pools on hard disks are skipped). An SSD with nothing mounted on it isn't trimmed; `sudo fstrim --all --verbose --dry-run` lists what will be. It's a storage job: its log, with how much each filesystem trimmed, is under **History → Jobs**. Debian's `fstrim.timer` also trims the ordinary filesystems weekly (`systemctl status fstrim.timer`).

**SMART names**: a disk's page names its attributes from smartctl's drive database. Debian's copy is as old as its release, so the agent installer updates it (`update-smart-drivedb`, then monthly with `homestack-drivedb.timer`), and adds Homestack's entries for drives it still gets wrong (`deploy/debian/agent/smart_drivedb.h`, for example every WD Blue SA510, where upstream only matches the 1 TB) as a marked block of `/etc/smart_drivedb.h`; the rest of that file is yours. An attribute still shown as Unknown on a disk that *is* in the database isn't publicly documented by its maker, like the Crucial BX500's 223, 249 and 251–254.

**Network**: **Networks → Server** in Homestack, or `networkctl` and `ip -br addr`. [`deploy/debian/network/`](deploy/debian/network/README.md) has an example of a bonded bridge with a VLAN for the VMs.

**Samba**: the **Shares** page holds the shares, who can use each, and the Samba users.

## Homestack

Homestack runs from its published image, and its settings are its container's `/data` folder. The host agent and its scripts install from a clone of the source (with the defaults, `/mnt/user/appdata/homestack-src`). To update, from that folder:

```bash
sudo git pull
sudo docker pull ghcr.io/leviathonbeast/homestack:latest
sudo docker rm -f homestack
sudo docker run -d --name homestack --restart unless-stopped --network host \
  -e HOMESTACK_STACKS=/mnt/user/appdata/stacks \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /run/homestack:/run/homestack \
  -v /mnt/user/appdata/homestack:/data \
  -v /mnt/user/appdata/stacks:/mnt/user/appdata/stacks \
  -v /var/run/libvirt:/var/run/libvirt \
  -v /mnt/user/isos:/mnt/user/isos:ro \
  ghcr.io/leviathonbeast/homestack:latest
sudo bash deploy/debian/agent/install.sh
```

Run the agent installer after every update. Storage warns when the agent and Homestack versions differ, and shows the command.

The helper scripts, each installed from the source folder:

| Installer | What it sets up |
| --- | --- |
| `deploy/debian/agent/install.sh` | The host agent: Storage, Fans, array devices, disk replacement, backups and Schedules; keeps smartctl's drive database current |
| `deploy/debian/notify/install.sh` | Disk, SMART, space, ZFS, mover and parity alerts, and the mover itself ([details](deploy/debian/notify/README.md)) |
| `deploy/debian/ups/install.sh` | Clean shutdown on a power cut, with alerts ([details](deploy/debian/ups/README.md)) |
| `deploy/debian/updates/install.sh` | Daily security updates only. Never the kernel, ZFS or Docker, and never a reboot |

Alert thresholds are in `/etc/default/homestack-alerts`; add mount points the space check should watch to `SPACE_MOUNTS` there. Set the BIOS's **Restore on AC/Power Loss** to **Power On**, so the server comes back by itself after a power cut.

## Parity

In Homestack's **Schedules** page:

- **Parity sync** runs nightly. It's skipped, with an alert, if more than 500 files were deleted or 1,000 modified since the last sync, so a mistake (or ransomware encrypting files) can't be written into parity. Storage shows why, with **Sync anyway**; from a shell, `sudo snapraid-job sync --force`. Set the limits in **Settings → Storage → Parity** (or `SNAPRAID_DELETE_LIMIT` and `SNAPRAID_UPDATE_LIMIT` in `/etc/default/homestack-alerts`); disk jobs use them too.
- **Parity check** runs weekly. By default each run checks 8% of the array, from blocks not checked for 10 days, so everything is checked about every three months. Both are in **Settings → Storage → Parity**, which writes them to `/etc/default/homestack-parity` for `snapraid-job`.

Whether a sync is needed is SnapRAID's call (`snapraid diff` exits with 2), not just the file counts: after an interrupted sync they can all be 0 while parity is unfinished, and the next sync completes it.

`snapraid-job` takes the storage lock, the same one Homestack's buttons and the mover use, so only one storage job runs at a time. Run `snapraid` by hand only when nothing else is running.

A sync or check started from Storage can be **paused** (for 1 to 8 hours, or until you resume it) and carries on from where it was: its processes are stopped in place, not ended. It keeps the storage lock meanwhile, so the mover skips its runs, and a scheduled parity job waits up to an hour and is then skipped with an alert. **Stop** ends it cleanly instead: SnapRAID saves its progress, and the next sync finishes the job. Restarting the agent stops a paused job cleanly too.

**SnapRAID's version matters for recovery.** A newer SnapRAID can write a content file, or have options in `snapraid.conf`, that an older one can't read. Debian's own package is older than the current release, so after reinstalling, install the same version as before (`snapraid --version`; backups record it and a restore warns when the server's is older). Each release has a `.deb` at [github.com/amadvance/snapraid/releases](https://github.com/amadvance/snapraid/releases).

**Snapshots** (SnapRAID 15 and on, `snapshot` in `snapraid.conf`): each sync leaves a `stable` snapshot of every data pool (under `.zfs/snapshot/`, hidden by default), and checks, scrubs and rebuilds read the other disks from it, so files changed or deleted since the last sync no longer spoil a recovery. The cost: deleted and changed files keep their space until the next sync replaces the snapshot. Run one sync after turning it on, so the snapshots exist.

SnapRAID 15.0 leaves its `snapraid-*` snapshots mounted after reading them (ZFS unmounts them after 5 idle minutes; a failed run leaves them on purpose), and ZFS counts a snapshot as mounted if it's mounted anywhere. **A container that binds a data disk** and starts while one is mounted keeps its own copy, because Docker binds are `rprivate` by default and the host's unmounts never reach it. SnapRAID then can't open that disk's snapshots after renaming them, and the sync stops before parity is touched with `Too many levels of symbolic links` (`Error stating pending snapshot '/mnt/diskN/.zfs/snapshot/'` or `Failed to replace pending snapshot`). `snapraid-job` and Homestack's disk jobs release SnapRAID's snapshot mounts before each run, on the host and in every container (`umount -N`), and retry a sync that still hits it once. The sync alert names any container that kept a copy: give its disk binds `:rslave` (for example `- /mnt/disk5:/mnt/disk5:ro,rslave`) so it follows the host's unmounts. By hand, `sudo grep -l 'snapshot/snapraid-' /proc/*/mounts` shows what holds one: unmount the host's (`findmnt -rn -o TARGET | grep '/.zfs/snapshot/snapraid-' | xargs -r -n1 sudo umount`), restart the container, and sync again.

## Storage jobs by hand

Homestack does all of these from **Storage**, with checks first. These are for when it can't. The examples use the default paths and disk 3; the commands at the top of each show where to read yours.

Before any of them, pause the mover and the parity schedules so they can't run into you, and remove the file once you're done:

```bash
echo "Rebuilding disk3 by hand" | sudo tee /var/lib/homestack-agent/storage-hold
sudo rm /var/lib/homestack-agent/storage-hold      # afterwards
```

Copy `/etc/fstab` and `/etc/snapraid.conf` before editing them, and check fstab with `sudo findmnt --verify` before rebooting. A new pool copies its `-O` settings from a working one: `zfs get -s local all disk1`. Below, they're the usual ones.

**Rebuild a dead data disk from parity.** You get back what was on it at the last sync.

```bash
grep -E '^data' /etc/snapraid.conf     # the disk's SnapRAID name (d3) and folder (/mnt/disk3/)
ls -l /dev/disk/by-id/ | grep -v -- -part   # the new disk: NEW below
sudo zpool export -f disk3 || sudo zpool destroy -f disk3
sudo zpool create -o ashift=12 -O compression=lz4 -O atime=off -O xattr=sa -O acltype=posixacl -m /mnt/disk3 disk3 /dev/disk/by-id/NEW
sudo zpool set cachefile=/etc/zfs/zpool.cache disk3
sudo snapraid -d d3 -l /var/log/snapraid-fix.log fix && sudo snapraid -d d3 -a check
sudo chown -R 99:100 /mnt/disk3        # the shares' owner: SnapRAID doesn't keep owners
sudo snapraid sync
```

**Replace a data disk that still works** (a bigger disk, or one failing SMART). Put the new disk in alongside the old one. It's copied while the disk stays in use, and parity isn't touched:

```bash
zpool status -P disk3                  # the old device, as ZFS lists it: OLD below
sudo zpool set autoexpand=on disk3
sudo zpool replace disk3 OLD /dev/disk/by-id/NEW
zpool status disk3                     # wait for the resilver to finish, then the old disk can go
```

**Add a data disk** (disk 6):

1. Create the pool: `sudo zpool create -o ashift=12 -O … -m /mnt/disk6 disk6 /dev/disk/by-id/NEW`, then `sudo zpool set cachefile=/etc/zfs/zpool.cache disk6`.
2. Create every share's folder on it, owned by the shares' owner.
3. Add `:/mnt/disk6` to both mergerfs lines in `/etc/fstab`, and to the running mounts:

   ```bash
   sudo apt install attr          # for setfattr
   for m in /mnt/user /mnt/user-array; do sudo setfattr -n user.mergerfs.branches -v '+>/mnt/disk6' "$m/.mergerfs"; done
   ```

4. Add `data d6 /mnt/disk6/` to `/etc/snapraid.conf`, then `sudo snapraid sync`.

**Format a parity disk** (for the next three jobs) the way Homestack does, with `parityN` for its folder's name:

```bash
findmnt -no FSTYPE /mnt/parity1        # the filesystem your parity disks use: ext4 or xfs
sudo parted -s -a optimal /dev/disk/by-id/NEW mklabel gpt mkpart parity2 ext4 0% 100%
sudo mkfs.ext4 -F -m 0 -T largefile4 -L parity2 /dev/disk/by-id/NEW-part1   # or: mkfs.xfs -f -L parity2 …
sudo blkid -s UUID -o value /dev/disk/by-id/NEW-part1   # its UUID, for fstab
```

**Move parity to a new disk:**

1. Format the new disk, and mount it somewhere temporary.
2. Copy `snapraid.parity` onto it.
3. Point the parity line in `/etc/fstab` at the new disk's UUID, and mount it at `/mnt/parity1`.
4. Run `sudo snapraid sync` to confirm. The old disk keeps its copy until you reuse it.

**Rebuild a dead parity disk.** Your data is unprotected until this finishes, so every data disk needs to be healthy.

1. Format the new disk, and point the parity fstab line at its UUID.
2. Run `sudo snapraid -d parity -l /var/log/snapraid-fix.log fix`. It takes about as long as reading your fullest disk.
3. Optionally check it with `sudo snapraid -d parity check`, then `sudo snapraid sync`.

**Add a second parity disk.** Format it, mount it at `/mnt/parity2` (`nofail`), add `2-parity /mnt/parity2/snapraid.2-parity` to `/etc/snapraid.conf`, then run `sudo snapraid sync -F`. That takes about a day, and the existing parity keeps protecting meanwhile.
