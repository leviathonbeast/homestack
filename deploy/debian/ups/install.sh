#!/bin/bash
# UPS (NUT): clean shutdown on a power cut, Discord/ntfy alerts, and
# the UPS on Homestack's Storage page. Run as root, after the alerts
# (deploy/debian/notify/install.sh):
#   sudo bash deploy/debian/ups/install.sh   (from Homestack's source folder)
set -euo pipefail
D=$(dirname "$(readlink -f "$0")")
[ "$(id -u)" = 0 ] || { echo "Run this with sudo."; exit 1; }

DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nut >/dev/null
[ -f /etc/default/homestack-ups ] || install -m 0644 "$D/homestack-ups.conf" /etc/default/homestack-ups
. /etc/default/homestack-ups

# 1. Keep the UPS awake on USB (autosuspend makes CyberPower units drop off;
#    the rule only matches CyberPower's USB ID).
cat > /etc/udev/rules.d/50-ups-no-autosuspend.rules <<'R'
ACTION=="add", SUBSYSTEM=="usb", ATTR{idVendor}=="0764", ATTR{idProduct}=="0501", TEST=="power/control", ATTR{power/control}="on"
R
udevadm control --reload-rules
udevadm trigger --subsystem-match=usb --attr-match=idVendor=0764 --action=add

# 2. NUT config. The monitor password is generated once and kept.
pw=$(sed -n 's/^[[:space:]]*password = //p' /etc/nut/upsd.users 2>/dev/null | head -1)
[ -n "$pw" ] || pw=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24)
echo 'MODE=standalone' > /etc/nut/nut.conf
sed -e "s/@LOW_CHARGE@/$UPS_LOW_CHARGE/" -e "s/@LOW_RUNTIME@/$UPS_LOW_RUNTIME/" "$D/ups.conf.in" > /etc/nut/ups.conf
printf 'LISTEN 127.0.0.1 3493\nLISTEN ::1 3493\n' > /etc/nut/upsd.conf
printf '[upsmon]\n  password = %s\n  upsmon primary\n' "$pw" > /etc/nut/upsd.users
sed "s/@PASSWORD@/$pw/" "$D/upsmon.conf.in" > /etc/nut/upsmon.conf
sed "s/@SHUTDOWN_AFTER@/$UPS_SHUTDOWN_AFTER/g" "$D/upssched.conf.in" > /etc/nut/upssched.conf
chown root:nut /etc/nut/*.conf /etc/nut/upsd.users
chmod 640 /etc/nut/*.conf /etc/nut/upsd.users

# 3. Scripts. The nut user may post alerts and start the shutdown (nothing else) through sudo.
install -m 0755 "$D/ups-event" /usr/local/sbin/ups-event
install -m 0755 "$D/ups-shutdown" /usr/local/sbin/ups-shutdown
printf 'nut ALL=(root) NOPASSWD: /usr/local/sbin/notify-discord, /usr/sbin/upsmon -c fsd\n' > /etc/sudoers.d/nut-notify
chmod 440 /etc/sudoers.d/nut-notify
visudo -cf /etc/sudoers.d/nut-notify >/dev/null

# 4. VMs shut down cleanly when the server powers off (instead of being killed).
if [ -f /etc/default/libvirt-guests ]; then
  sed -i -E 's/^#?ON_BOOT=.*/ON_BOOT=ignore/; s/^#?ON_SHUTDOWN=.*/ON_SHUTDOWN=shutdown/; s/^#?SHUTDOWN_TIMEOUT=.*/SHUTDOWN_TIMEOUT=120/' /etc/default/libvirt-guests
  systemctl enable --now libvirt-guests >/dev/null 2>&1 || true
fi

# 5. Start.
systemctl restart nut-driver-enumerator.service 2>/dev/null || true
systemctl restart nut-driver.target nut-server.service
systemctl enable --now nut-monitor.service >/dev/null 2>&1
systemctl restart nut-monitor.service
sleep 5

echo
if upsc cyberpower@localhost ups.status >/dev/null 2>&1; then
  echo "UPS: $(upsc cyberpower@localhost device.model 2>/dev/null) · status $(upsc cyberpower@localhost ups.status) · battery $(upsc cyberpower@localhost battery.charge)% · runtime $(( $(upsc cyberpower@localhost battery.runtime 2>/dev/null || echo 0) / 60 )) min · load $(upsc cyberpower@localhost ups.load)%"
  echo "Shuts down after $((UPS_SHUTDOWN_AFTER / 60)) min on battery, or at ${UPS_LOW_CHARGE}% / $((UPS_LOW_RUNTIME / 60)) min left."
else
  echo "NUT can't read the UPS yet. Check: journalctl -u nut-driver@cyberpower -n 30"
fi
echo "Test the alert (nothing shuts down): sudo -u nut /usr/local/sbin/ups-event onbatt"
