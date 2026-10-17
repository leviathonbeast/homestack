# Example network config (systemd-networkd)

An example of moving an Unraid server's network to Debian as it was, with
systemd-networkd:

- two ports → bond0 (active-backup)
- bond0 → bridge br0 at 10.0.10.5/24 and 2001:db8:0:8a00::5/64
- VLAN 40 → bridge br0.40 for the VMs, with no host address

Change the MAC addresses (`10-bond0-ports.network`, `10-bond0.netdev`), the
addresses, gateway and DNS (`20-br0.network`) and the VLAN to yours first. With
a single port, leave out the bond files and put `Bridge=br0` in a `.network`
that matches the port. Once networkd runs the network, Homestack's
**Networks → Server** page can change addresses, bonds and VLANs for you.

Apply it from the console or IPMI (not over SSH), because the network restarts:

```bash
sudo apt install systemd-resolved
sudo cp *.netdev *.network /etc/systemd/network/
# Hand the network over from ifupdown to systemd-networkd
sudo mv /etc/network/interfaces /etc/network/interfaces.installer
printf 'auto lo\niface lo inet loopback\n' | sudo tee /etc/network/interfaces
sudo systemctl disable networking
sudo systemctl enable --now systemd-networkd systemd-resolved
sudo reboot
```

Check afterwards:

```bash
networkctl                 # bond0, br0, bond0.40, br0.40 all "routable" or "configured"
ip -br addr show br0       # your addresses
cat /proc/net/bonding/bond0
ping -c3 <your gateway> && ping -6 -c3 2606:4700:4700::1111
```

To undo: `sudo mv /etc/network/interfaces.installer /etc/network/interfaces && sudo systemctl enable networking && sudo systemctl disable systemd-networkd && sudo reboot`.
