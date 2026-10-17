#!/bin/bash
# Automatic security updates, with Discord reports. Run as root,
# after the alerts (deploy/debian/notify/install.sh) are installed:
#   sudo bash deploy/debian/updates/install.sh   (from Homestack's source folder)
set -euo pipefail
D=$(dirname "$(readlink -f "$0")")
[ "$(id -u)" = 0 ] || { echo "Run this with sudo."; exit 1; }

DEBIAN_FRONTEND=noninteractive apt-get install -y -qq unattended-upgrades apt-listchanges needrestart >/dev/null

install -m 0644 "$D/20auto-upgrades" /etc/apt/apt.conf.d/20auto-upgrades
install -m 0644 "$D/52homestack-updates" /etc/apt/apt.conf.d/52homestack-updates
install -D -m 0644 "$D/needrestart-homestack.conf" /etc/needrestart/conf.d/homestack.conf
install -m 0755 "$D/updates-notify" /usr/local/sbin/updates-notify

# Report to Discord after each daily run.
install -d /etc/systemd/system/apt-daily-upgrade.service.d
cat > /etc/systemd/system/apt-daily-upgrade.service.d/homestack-notify.conf <<'CONF'
[Service]
ExecStartPost=-/usr/local/sbin/updates-notify
CONF
systemctl daemon-reload
systemctl enable --now apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1

echo "Checking the settings (nothing is installed):"
unattended-upgrade --dry-run --debug 2>&1 | grep -E "Allowed origins|Initial blacklist|Packages that will be upgraded" || true
echo
echo "Done. Updates run daily ($(systemctl show -p NextElapseUSecRealtime --value apt-daily-upgrade.timer 2>/dev/null || echo 'see systemctl list-timers')); results go to Discord."
