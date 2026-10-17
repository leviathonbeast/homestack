#!/bin/bash
# Installs the server alert jobs. Run as root on Debian, after Homestack is
# running and Discord is connected in Settings → Notifications:
#   sudo bash deploy/debian/notify/install.sh   (from Homestack's source folder)
set -euo pipefail
D=$(dirname "$(readlink -f "$0")")
[ "$(id -u)" = 0 ] || { echo "Run this with sudo."; exit 1; }

apt-get install -y -qq jq smartmontools >/dev/null

install -m 0755 "$D/notify-discord"   /usr/local/sbin/notify-discord
install -m 0755 "$D/disk-temp-check"  /usr/local/sbin/disk-temp-check
install -m 0755 "$D/smart-check"      /usr/local/sbin/smart-check
install -m 0755 "$D/space-check"      /usr/local/sbin/space-check
install -m 0755 "$D/mover"            /usr/local/sbin/mover
install -m 0755 "$D/snapraid-job"     /usr/local/sbin/snapraid-job
install -D -m 0644 "$D/alerts-lib.sh" /usr/local/lib/homestack/alerts-lib.sh
if grep -q '^# Managed by Homestack' /etc/cron.d/homestack-alerts 2>/dev/null; then
  echo "Kept your schedules (edited on Homestack's Schedules page)."
else
  install -m 0644 "$D/homestack-alerts.cron" /etc/cron.d/homestack-alerts
fi
install -d -m 0755 /var/lib/homestack-alerts
if [ -f /etc/default/homestack-alerts ]; then
  echo "Kept your existing /etc/default/homestack-alerts."
else
  install -m 0644 "$D/homestack-alerts.conf" /etc/default/homestack-alerts
fi

# An older mover line in /etc/crontab: the cron file above runs it now.
sed -i '\|/usr/local/sbin/mover|d' /etc/crontab

# ZFS: problems, scrub start/finish/stop, rebuilds.
install -m 0755 "$D/zed-homestack.sh" /etc/zfs/zed.d/all-homestack.sh
systemctl restart zfs-zed

# SMART: health failures, failed self-tests, new errors, unreachable disks,
# plus a short self-test every Sunday at 02:00. Sleeping disks aren't woken.
# (Temperatures and sector counts are watched by disk-temp-check and smart-check.)
install -D -m 0755 "$D/smartd-homestack" /etc/smartmontools/run.d/60homestack
if ! grep -q '^# homestack' /etc/smartd.conf; then
  cp /etc/smartd.conf /etc/smartd.conf.before-homestack
  sed -i 's/^DEVICESCAN/#&/' /etc/smartd.conf
  cat >> /etc/smartd.conf <<'CONF'
# homestack: SMART warnings go to Discord through /etc/smartmontools/run.d/60homestack
DEVICESCAN -d removable -n standby,q -H -f -l error -l selftest -s S/../../7/02 -m root -M exec /usr/share/smartmontools/smartd-runner
CONF
fi
systemctl restart smartmontools

# A copy of the Homestack binary, so alerts still go out while its container
# is down (for example during an update). HOMESTACK_CONTAINER: its name, if
# not homestack.
C=${HOMESTACK_CONTAINER:-homestack}
if docker inspect "$C" >/dev/null 2>&1; then
  install -d /usr/local/lib/homestack
  docker cp "$C":/usr/local/bin/homestack /usr/local/lib/homestack/homestack >/dev/null && chmod 0755 /usr/local/lib/homestack/homestack
fi

echo "Installed. Sending a test message..."
if notify-discord -l success -t "Server alerts installed" \
  "ZFS, SMART, disk temperature, disk space, mover and parity alerts are set up on $(hostname). Settings: /etc/default/homestack-alerts"; then
  echo "Sent. Check your Discord channel."
else
  echo "Couldn't send. Is Homestack running, with Discord connected and \"Server alerts\" switched on?"
fi
