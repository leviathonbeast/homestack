# UPS (NUT): clean shutdown on a power cut

```bash
sudo bash deploy/debian/ups/install.sh
```

For the CyberPower VP1600ELCD on USB. What happens in a power cut:

| When | What happens |
| --- | --- |
| Power goes off | Discord/ntfy: "Power cut: running on battery", with charge, runtime and load. A 5-minute timer starts |
| Power comes back within 5 min | "Power is back"; the timer is cancelled, nothing shuts down |
| 5 min on battery, **or** battery at 40% / 7 min runtime left | "Shutting down". A parity job is stopped cleanly (it resumes next time), then the server powers off: containers stop, VMs shut down (libvirt-guests, up to 2 min), pools are unmounted and exported |
| At the very end of the shutdown | NUT tells the UPS to switch its outlets off, so the server gets a clean power cycle and boots again when mains returns |

For the last row to bring the server back, set the BIOS option **Restore on AC/Power Loss** to **Power On**.

Other alerts: lost contact with the UPS for 5 minutes (then "reconnected"), and "battery needs replacing". The Storage page shows the UPS tile (mains/battery, charge, runtime, load).

| File | Installed as |
| --- | --- |
| `homestack-ups.conf` | `/etc/default/homestack-ups`: the timer and low-battery limits. Edit, then re-run the installer |
| `ups.conf.in`, `upsmon.conf.in`, `upssched.conf.in` | `/etc/nut/` (generated; `upsd.conf` listens on localhost only) |
| `ups-event` | `/usr/local/sbin/`: alerts, and starts the shutdown when the timer runs out |
| `ups-shutdown` | `/usr/local/sbin/`: upsmon's shutdown command |

Also installed: a udev rule that stops USB power-saving on the UPS (CyberPower units otherwise drop off USB every few seconds), a sudoers entry letting the `nut` user post alerts and start the shutdown, and `ON_SHUTDOWN=shutdown` in `/etc/default/libvirt-guests`.

## Check and test

```bash
upsc cyberpower@localhost                     # everything the UPS reports
upsc cyberpower@localhost ups.status          # OL = on mains, OB = on battery, LB = battery low
sudo -u nut /usr/local/sbin/ups-event onbatt  # test alert only: nothing shuts down
journalctl -u nut-monitor -u nut-driver@cyberpower -n 30
```

A real test (pull the UPS's mains plug for a minute, then plug it back in) should give "Power cut" then "Power is back" and no shutdown. Only do it when no flatten, parity sync or scrub is running.
