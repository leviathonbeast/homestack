package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"homestack/internal/notify"
	"homestack/internal/state"
)

// The array here is the ZFS data pools plus the mergerfs views on top of them.
// The cache stays mounted when the array is stopped: Homestack itself and its
// settings live there, so you can start the array again from the web page.

type arrayMemory struct {
	Containers []string `json:"containers"`
	VMs        []string `json:"vms"`
}

func (a *Agent) saveMemory(m arrayMemory) {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(filepath.Join(a.opt.StateDir, "array-stopped.json"), b, 0o640)
}

func (a *Agent) loadMemory() arrayMemory {
	var m arrayMemory
	b, err := os.ReadFile(filepath.Join(a.opt.StateDir, "array-stopped.json"))
	if err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (a *Agent) arrayPools(ctx context.Context) []Pool {
	var out []Pool
	for _, p := range a.pools(ctx) {
		if p.Array {
			out = append(out, p)
		}
	}
	return out
}

// stopServices stops containers (except Homestack), VMs and Samba, and
// returns what was running so it can be started again.
func (a *Agent) stopServices(ctx context.Context, log func(string), forceVMs bool) (arrayMemory, error) {
	var mem arrayMemory
	if out, err := a.cmd(ctx, 30*time.Second, "docker", "ps", "--format", "{{.Names}}"); err == nil {
		for n := range strings.FieldsSeq(out) {
			if n != "homestack" {
				mem.Containers = append(mem.Containers, n)
			}
		}
	}
	if len(mem.Containers) > 0 {
		log(fmt.Sprintf("Stopping %d containers", len(mem.Containers)))
		if out, err := a.cmd(ctx, 5*time.Minute, "docker", append([]string{"stop", "-t", "60"}, mem.Containers...)...); err != nil {
			return mem, fmt.Errorf("couldn't stop every container: %s", strings.TrimSpace(out))
		}
	}
	if out, err := a.cmd(ctx, 20*time.Second, "virsh", "-c", "qemu:///system", "list", "--name", "--state-running"); err == nil {
		mem.VMs = strings.Fields(out)
	}
	for _, vm := range mem.VMs {
		log("Shutting down VM " + vm)
		_, _ = a.cmd(ctx, 20*time.Second, "virsh", "-c", "qemu:///system", "shutdown", vm)
	}
	if len(mem.VMs) > 0 {
		deadline := time.Now().Add(150 * time.Second)
		for time.Now().Before(deadline) {
			out, _ := a.cmd(ctx, 20*time.Second, "virsh", "-c", "qemu:///system", "list", "--name", "--state-running")
			if strings.TrimSpace(out) == "" {
				break
			}
			time.Sleep(5 * time.Second)
		}
		if out, _ := a.cmd(ctx, 20*time.Second, "virsh", "-c", "qemu:///system", "list", "--name", "--state-running"); strings.TrimSpace(out) != "" {
			still := strings.Fields(out)
			if !forceVMs {
				return mem, fmt.Errorf("these VMs didn't shut down within 2½ minutes: %s. Shut them down from inside, or use Force", strings.Join(still, ", "))
			}
			for _, vm := range still {
				log("Forcing VM " + vm + " off")
				_, _ = a.cmd(ctx, 20*time.Second, "virsh", "-c", "qemu:///system", "destroy", vm)
			}
		}
	}
	log("Stopping Samba")
	_, _ = a.cmd(ctx, 60*time.Second, "systemctl", "stop", "smbd")
	return mem, nil
}

func (a *Agent) whoUses(ctx context.Context, path string) string {
	out, _ := a.cmd(ctx, 10*time.Second, "fuser", "-vm", path)
	var names []string
	for _, line := range strings.Split(out, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 1 && f[len(f)-1] != "mount" {
			names = append(names, f[len(f)-1])
		}
	}
	if len(names) == 0 {
		return "something"
	}
	return strings.Join(names, ", ")
}

func (a *Agent) arrayStop(ctx context.Context, log func(string), force bool) error {
	a.notifyHost(notify.Warning, "Stopping the array", "Stopping containers, VMs and Samba, then unmounting the data disks.")
	mem, err := a.stopServices(ctx, log, force)
	a.saveMemory(mem)
	if err != nil {
		return err
	}
	log("Writing everything to disk")
	_, _ = a.cmd(ctx, 5*time.Minute, "sync")
	// mergerfs views first (top of the stack), then data disks that aren't
	// ZFS (ext4, XFS…), then the pools.
	mounts := a.layout().arrayMounts()
	plain := a.plainDataDisks(ctx)
	order := slices.Clone(mounts)
	slices.Reverse(order)
	for _, m := range append(order, plain...) {
		if !a.isMounted(ctx, m) {
			continue
		}
		log("Unmounting " + m)
		if out, err := a.cmd(ctx, 60*time.Second, "umount", m); err != nil {
			return fmt.Errorf("%s is still in use by %s (%s). Nothing further was changed", m, a.whoUses(ctx, m), strings.TrimSpace(out))
		}
	}
	for _, p := range a.arrayPools(ctx) {
		if !p.Mounted {
			continue
		}
		log("Unmounting pool " + p.Name)
		if out, err := a.cmd(ctx, 2*time.Minute, "zfs", "unmount", p.Name); err != nil {
			return fmt.Errorf("pool %s is still in use by %s (%s)", p.Name, a.whoUses(ctx, p.Mountpoint), strings.TrimSpace(out))
		}
	}
	// Make the empty mount points read-only, so nothing can write into them
	// (onto the boot disk) while the array is stopped.
	for _, p := range a.arrayPools(ctx) {
		if p.Mountpoint != "" && p.Mountpoint != "none" && !a.isMounted(ctx, p.Mountpoint) {
			_, _ = a.cmd(ctx, 10*time.Second, "chattr", "+i", p.Mountpoint)
		}
	}
	for _, m := range append(mounts, plain...) {
		if !a.isMounted(ctx, m) {
			_, _ = a.cmd(ctx, 10*time.Second, "chattr", "+i", m)
		}
	}
	a.writeTime("array-stopped")
	log("Array stopped. The pools stay imported; the cache and Homestack keep running.")
	a.notifyHost(notify.Info, "Array stopped", "The data disks are unmounted. Start the array from Homestack's Storage page.")
	return nil
}

func (a *Agent) arrayStart(ctx context.Context, log func(string)) error {
	for _, p := range a.arrayPools(ctx) {
		if p.Mounted {
			continue
		}
		log("Mounting pool " + p.Name)
		if out, err := a.cmd(ctx, 2*time.Minute, "zfs", "mount", p.Name); err != nil {
			return fmt.Errorf("couldn't mount %s: %s", p.Name, strings.TrimSpace(out))
		}
	}
	for _, p := range a.arrayPools(ctx) {
		if !p.Mounted {
			return fmt.Errorf("pool %s still isn't mounted; not going further", p.Name)
		}
	}
	// Data disks that aren't ZFS (ext4, XFS…), from fstab, before the
	// mergerfs views over them.
	for _, m := range a.plainDataDisks(ctx) {
		if a.isMounted(ctx, m) {
			continue
		}
		log("Mounting " + m)
		if out, err := a.cmd(ctx, 60*time.Second, "mount", m); err != nil {
			return fmt.Errorf("couldn't mount %s: %s", m, strings.TrimSpace(out))
		}
	}
	for _, m := range a.layout().arrayMounts() {
		if a.isMounted(ctx, m) {
			continue
		}
		log("Mounting " + m)
		if out, err := a.cmd(ctx, 60*time.Second, "mount", m); err != nil {
			return fmt.Errorf("couldn't mount %s: %s", m, strings.TrimSpace(out))
		}
	}
	log("Starting Samba")
	_, _ = a.cmd(ctx, 60*time.Second, "systemctl", "start", "smbd")
	mem := a.loadMemory()
	vms := mem.VMs
	if len(vms) == 0 {
		out, _ := a.cmd(ctx, 20*time.Second, "virsh", "-c", "qemu:///system", "list", "--name", "--autostart", "--inactive")
		vms = strings.Fields(out)
	}
	for _, vm := range vms {
		log("Starting VM " + vm)
		_, _ = a.cmd(ctx, 60*time.Second, "virsh", "-c", "qemu:///system", "start", vm)
	}
	if len(mem.Containers) > 0 {
		log(fmt.Sprintf("Starting %d containers", len(mem.Containers)))
		_, _ = a.cmd(ctx, 10*time.Minute, "docker", append([]string{"start"}, mem.Containers...)...)
	}
	_ = os.Remove(filepath.Join(a.opt.StateDir, "array-stopped"))
	_ = os.Remove(filepath.Join(a.opt.StateDir, "array-stopped.json"))
	log("Array started.")
	a.notifyHost(notify.Success, "Array started", "Data disks mounted; containers, VMs and Samba are back.")
	return nil
}

func (a *Agent) powerOff(ctx context.Context, log func(string), action string) error {
	verb := map[string]string{"reboot": "Rebooting", "shutdown": "Shutting down"}[action]
	a.notifyHost(notify.Warning, verb+" the server", "Stopping containers, VMs and Samba first.")
	if _, err := a.stopServices(ctx, log, true); err != nil {
		log("Warning: " + err.Error() + " (continuing; the system shutdown will finish the job)")
	}
	log("Writing everything to disk")
	_, _ = a.cmd(ctx, 5*time.Minute, "sync")
	log(verb + " now")
	cmd := map[string]string{"reboot": "reboot", "shutdown": "poweroff"}[action]
	if out, err := a.cmd(ctx, 60*time.Second, "systemctl", cmd); err != nil {
		return fmt.Errorf("systemctl %s: %s", cmd, strings.TrimSpace(out))
	}
	return nil
}

// notifyHost posts to Discord using Homestack's saved settings, if it can
// read them (they're on the cache).
func (a *Agent) notifyHost(level notify.Level, title, body string) {
	if a.opt.DataDir == "" {
		return
	}
	path := filepath.Join(a.opt.DataDir, "homestack.json")
	if _, err := os.Stat(path); err != nil {
		return
	}
	st, err := state.Open(path, "")
	if err != nil {
		return
	}
	cfg := st.Notifications().NotifyConfig()
	if !cfg.Active() || !cfg.EventEnabled("host") {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_, _ = notify.DeliverNow(ctx, cfg, notify.Message{Event: "host", Level: level, Title: title, Body: body})
	}()
}
