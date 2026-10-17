# Server alerts to Discord

Homestack already reports container, stack, update and VM events (**Settings → Notifications**). These scripts add the alerts Unraid used to give you, posted to the same Discord channel through `homestack notify`, so there's only one webhook to manage.

**Backup channel:** set up [ntfy](https://ntfy.sh) in the same settings page and every alert (these scripts included) goes to the ntfy phone app when Discord can't be reached, or also for warnings and errors if you choose. Nothing to change here: the scripts go through `homestack notify`, which handles both.

## Install

1. Connect Discord in **Settings → Notifications**, and leave **Server alerts** switched on.
2. Run the installer, and check for its test message in Discord:
   ```bash
   sudo bash deploy/debian/notify/install.sh
   ```

Settings (temperature limits, full-disk limit, mover shares, SnapRAID limits) are in `/etc/default/homestack-alerts`. Each job reads the file every time it runs, so edits take effect without restarting anything.

## What's included

| File | Installed as | What it does |
| --- | --- | --- |
| `notify-discord` | `/usr/local/sbin/` | Posts a message. Goes through Homestack's container, or a copy of the binary if the container is down |
| `disk-temp-check` | `/usr/local/sbin/`, every 10 min | Reports a disk at its warning or critical temperature, and again when it's back to normal. Limits are separate for hard drives (45/50°C), SATA SSDs (60/70°C) and NVMe (65/75°C) |
| `smart-check` | `/usr/local/sbin/`, hourly | Watches the SMART values that predict failure and explains any change in plain English, with what to do next |
| `smartd-homestack` | `/etc/smartmontools/run.d/60homestack` | Explains smartd's alerts: failed health check, failed self-test, new errors, disk unreachable. smartd also runs a short self-test every Sunday |
| `space-check` | `/usr/local/sbin/`, hourly | Reports a filesystem or ZFS pool over 90% full, and any pool that isn't healthy |
| `zed-homestack.sh` | `/etc/zfs/zed.d/all-homestack.sh` | ZFS scrub started, finished or stopped; rebuilds; faulted disks; checksum and I/O errors (at most one per pool an hour) |
| `mover` | `/usr/local/sbin/`, 03:00 nightly | Moves files older than a day from the cache to the array, then posts a summary per share (like Mover Tuning) |
| `snapraid-job` | `/usr/local/sbin/` (cron lines start switched off) | Parity sync and check, reported when they start, finish or fail. A sync is skipped if more than 500 files were deleted or 1,000 modified since the last one (`SNAPRAID_DELETE_LIMIT`, `SNAPRAID_UPDATE_LIMIT`); whether one is needed is `snapraid diff`'s exit code, so an interrupted sync is always completed |

Sleeping disks are never woken by these checks.

## Tuned for your drives

- **Seagate (IronWolf, IronWolf Pro, Exos):** attributes 1, 7 and 195 pack several counters into huge raw numbers that look alarming but are normal, so they're ignored. 187 (Reported_Uncorrect) is watched, because on Seagate it's one of the best early signs of failure.
- **WD hard drives (Red, Red Plus):** 1 (Raw_Read_Error_Rate) is a real count that should stay at 0, and 200 (Multi_Zone_Error_Rate) counts write errors. Both are watched.
- **Samsung SATA SSDs:** wear comes from Wear_Leveling_Count (177). Failed flash blocks come from Used_Rsvd_Blk_Cnt_Tot, Runtime_Bad_Block, and program and erase failures.
- **Crucial SATA SSDs (MX, BX):** wear comes from Percent_Lifetime_Remain (202), or the drive's own endurance statistic. MX500 firmware is known to flick 197 between 0 and 1, so a single pending sector on a Crucial SSD is ignored.
- **NVMe (Crucial P-series, Samsung 9x0…):** the standard NVMe health log is used: media errors, spare blocks, critical warnings and wear.
- **Every hard drive:** reallocated (5), pending (197) and uncorrectable (198) sectors; cable errors (199), reported as a cable problem rather than a failing disk; and head parking (193) passing 300,000 cycles.

The first `smart-check` run reports any counter that's already above zero, as a baseline. After that, only changes are reported.

## Sending your own alerts

```bash
notify-discord -l warning -t "Backup" "Nightly backup took longer than usual"
some-job || notify-discord -l error -t "some-job failed" "Check the log"
```

Levels are `info`, `success`, `warning` and `error`. The message can also come from stdin.
