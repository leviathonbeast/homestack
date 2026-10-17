#!/bin/bash
# Installs (or updates) the Homestack host agent, which powers the Storage,
# Fans and Networks → Server pages. Run as root on Debian, with the Homestack
# container running, from Homestack's source folder:
#   sudo bash deploy/debian/agent/install.sh
# Run it again after every Homestack update so the agent matches. If your
# Homestack container has another name: sudo HOMESTACK_CONTAINER=name bash …
set -euo pipefail
D=$(dirname "$(readlink -f "$0")")
SRC=$(readlink -f "$D/../../..")
[ "$(id -u)" = 0 ] || { echo "Run this with sudo."; exit 1; }
C=${HOMESTACK_CONTAINER:-homestack}

docker inspect "$C" >/dev/null 2>&1 || { echo "There's no container called $C. Start Homestack first (or set HOMESTACK_CONTAINER to its name)."; exit 1; }

# Where the storage is: the layout the agent last wrote (Settings → Storage),
# or Unraid's paths before it has.
STORAGE_CACHE=/mnt/cache STORAGE_APPDATA=/mnt/user/appdata
# shellcheck disable=SC1091
[ -r /etc/default/homestack-layout ] && . /etc/default/homestack-layout

# Homestack's settings folder: the one its container has as /data. When that's
# inside the apps' data mount (which goes away while the array is stopped),
# the agent uses the same folder on the cache, which it's bound from.
data=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}' "$C")
if [ -n "$data" ] && [ -n "$STORAGE_APPDATA" ] && [[ $data == "$STORAGE_APPDATA"/* ]]; then
  a_src=$(findmnt -no SOURCE --mountpoint "$STORAGE_APPDATA" 2>/dev/null | sed 's/\[.*//') || true
  c_src=$(findmnt -no SOURCE --mountpoint "$STORAGE_CACHE" 2>/dev/null | sed 's/\[.*//') || true
  a_root=$(findmnt -no FSROOT --mountpoint "$STORAGE_APPDATA" 2>/dev/null) || true
  c_root=$(findmnt -no FSROOT --mountpoint "$STORAGE_CACHE" 2>/dev/null) || true
  c_root=${c_root%/}
  if [ -n "$a_src" ] && [ "$a_src" = "$c_src" ] && [[ $a_root == "$c_root"/* ]]; then
    data="$STORAGE_CACHE/${a_root#"$c_root"/}/${data#"$STORAGE_APPDATA"/}"
  fi
fi

# 1. The binary, copied out of the running container (it's static, so it runs
#    on the host as is). The notify scripts use the same copy.
install -d /usr/local/lib/homestack
docker cp "$C":/usr/local/bin/homestack /usr/local/lib/homestack/homestack.new >/dev/null
chmod 0755 /usr/local/lib/homestack/homestack.new
mv -f /usr/local/lib/homestack/homestack.new /usr/local/lib/homestack/homestack
echo "Binary: $(/usr/local/lib/homestack/homestack version 2>/dev/null || echo installed)"

# The mover and parity job scripts change with the agent (e.g. they pause
# while a disk is being replaced), so refresh them too when installed.
for s in mover snapraid-job; do
  if [ -e /usr/local/sbin/$s ] && [ -f "$D/../notify/$s" ]; then
    install -m 0755 "$D/../notify/$s" /usr/local/sbin/$s
  fi
done

# The nightly backup's command (Schedules → Backup).
install -m 0755 "$D/homestack-backup" /usr/local/sbin/homestack-backup

# 2. The Fans page needs the driver for the motherboard's sensor chip. Most
#    boards from the last few years have a Nuvoton chip (nct6775 driver):
#    it's loaded at every boot only if it finds one here.
if modprobe nct6775 2>/dev/null; then
  grep -qx nct6775 /etc/modules-load.d/*.conf 2>/dev/null || echo nct6775 > /etc/modules-load.d/homestack-fans.conf
elif ! ls /sys/class/hwmon/*/pwm1 >/dev/null 2>&1; then
  echo "Note: no fan controls found. If the Fans page is empty, find your board's sensor driver with sensors-detect (sudo apt install lm-sensors)."
fi

# The Networks page tests every link's gateway with ping after a change, and
# undoes the change if one stops answering.
if ! command -v ping >/dev/null; then
  apt-get install -y -qq iputils-ping >/dev/null 2>&1 || echo "Note: install iputils-ping, so network changes can be tested."
fi

# Storage → Array devices partitions a new parity disk with parted.
if ! command -v parted >/dev/null; then
  apt-get install -y -qq parted >/dev/null 2>&1 || echo "Note: install parted, so parity disks can be added from the Storage page."
fi

# Spin-down (Settings → Storage) puts idle hard disks in standby with hdparm.
if ! command -v hdparm >/dev/null; then
  apt-get install -y -qq hdparm >/dev/null 2>&1 || echo "Note: install hdparm, so idle disks can be spun down."
fi

# SMART attribute names (a disk's page) come from smartctl's drive database.
# Debian's copy is as old as its release, so update it now and monthly
# (homestack-drivedb.timer); update-smart-drivedb checks its signature with gpg.
if command -v update-smart-drivedb >/dev/null; then
  command -v gpg >/dev/null || apt-get install -y -qq gnupg >/dev/null 2>&1 || true
  if update-smart-drivedb >/dev/null 2>&1; then
    echo "SMART drive database: updated."
  else
    echo "Note: couldn't update smartctl's drive database (sudo update-smart-drivedb says why); some attribute names may show as Unknown."
  fi
  install -m 0644 "$D/homestack-drivedb.service" "$D/homestack-drivedb.timer" /etc/systemd/system/
  systemctl daemon-reload
  systemctl enable --now homestack-drivedb.timer >/dev/null 2>&1 || true
fi
# Drives that database still gets wrong ($D/smart_drivedb.h), as a marked
# block of /etc/smart_drivedb.h, which smartctl reads first. The rest of that
# file is left as it is, and the block is only used if smartctl reads it.
if command -v smartctl >/dev/null; then
  begin='/* BEGIN Homestack: written by deploy/debian/agent/install.sh, replaced on every run */'
  end='/* END Homestack */'
  tmp=$(mktemp)
  {
    [ -f /etc/smart_drivedb.h ] && awk -v b="$begin" -v e="$end" '$0 == b { skip = 1 } !skip { print } $0 == e { skip = 0 }' /etc/smart_drivedb.h
    echo "$begin"
    cat "$D/smart_drivedb.h"
    echo "$end"
  } > "$tmp"
  check=$(smartctl -B "+$tmp" -P showall "WD Blue SA510 2.5 2000GB" 2>&1 || true)
  if grep -q "MODEL FAMILY: *WD Blue SA510 SSDs" <<<"$check"; then
    install -m 0644 "$tmp" /etc/smart_drivedb.h
  else
    echo "Note: smartctl couldn't read Homestack's drive entries, so /etc/smart_drivedb.h was left as it was:"
    grep -iE "error|syntax" <<<"$check" | head -3 || true
  fi
  rm -f "$tmp"
fi

# 3. The service, told where Homestack's settings and source are (% is
#    special in unit files).
install -m 0644 "$D/homestack-agent.service" /etc/systemd/system/homestack-agent.service
install -d /etc/systemd/system/homestack-agent.service.d
{
  echo "# Written by deploy/debian/agent/install.sh: where Homestack's settings and source are."
  echo "[Service]"
  [ -n "$data" ] && echo "Environment=\"HOMESTACK_DATA=${data//%/%%}\""
  echo "Environment=\"HOMESTACK_SRC=${SRC//%/%%}\""
} > /etc/systemd/system/homestack-agent.service.d/paths.conf
echo "Homestack's settings: ${data:-not found (is /data mounted in its container?)}"
systemctl daemon-reload
systemctl enable homestack-agent >/dev/null 2>&1
systemctl restart homestack-agent
sleep 2
if systemctl is-active -q homestack-agent && [ -S /run/homestack/agent.sock ]; then
  echo "Agent running."
else
  echo "The agent didn't start. See: journalctl -u homestack-agent -n 50"; exit 1
fi

# 4. Homestack needs /run/homestack mounted to reach the agent.
if docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$C" | grep -qw /run/homestack; then
  echo "Homestack can see the agent. Open Storage and Fans in the sidebar (reload the page)."
else
  cat <<MSG

One more step: Homestack's container needs the agent's folder. Recreate it
with this extra line in its docker run (or compose volumes):
    -v /run/homestack:/run/homestack
Your settings are in ${data:-its /data folder}, so nothing is lost.
MSG
fi
