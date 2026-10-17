package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"homestack/internal/backup"
)

const testPass = "correct horse battery staple"

// backupDir is the default backup folder with the default layout.
const backupDir = "/mnt/user/backups/homestack"

const win11XML = `<domain type='kvm'><name>win11</name><os><loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.ms.fd</loader><nvram template='/usr/share/OVMF/OVMF_VARS_4M.ms.fd'>/var/lib/libvirt/qemu/nvram/win11_VARS.fd</nvram></os></domain>`

// backupHost is assignHost plus what a backup reads: Homestack's settings,
// Samba, the alerts, fans, the UPS, two VMs and the network files.
func backupHost(t *testing.T) (*Agent, *hostRunner) {
	t.Helper()
	a, r, _ := assignHost(t)
	put := func(p, s string) {
		os.MkdirAll(filepath.Dir(r.path(p)), 0o755)
		os.WriteFile(r.path(p), []byte(s), 0o640)
	}
	data := t.TempDir()
	a.opt.DataDir = data
	os.WriteFile(filepath.Join(data, "homestack.json"), []byte(`{"version":1,"admin":{"username":"andrew","passwordHash":"$x"},"sessions":[{"id":"s1","tokenHash":"secret-session"}],"settings":{"appHost":"lucy.lan"}}`), 0o600)
	a.opt.NetDir = r.path("etc/systemd/network")
	t.Setenv("HOMESTACK_SMB_CONF", r.path("etc/samba/smb.conf"))
	t.Setenv("HOMESTACK_AGENT_CRON", r.path("etc/cron.d/homestack-alerts"))
	t.Setenv("HOMESTACK_ALERTS_CONF", r.path("etc/default/homestack-alerts"))
	put("etc/samba/smb.conf", "[global]\n   include = /etc/samba/homestack-shares.conf\n")
	put("etc/samba/homestack-shares.conf", "[Media]\n   path = /mnt/user/Media\n")
	put("var/lib/samba/private/passdb.tdb", "TDB passwords")
	put("etc/cron.d/homestack-alerts", "17 * * * * root space-check\n")
	put("etc/default/homestack-alerts", "DISCORD_WEBHOOK=https://discord/x\n")
	put("etc/modprobe.d/zfs.conf", "options zfs zfs_arc_max=17179869184\n")
	put("etc/modules-load.d/homestack-fans.conf", "nct6775\n")
	put("etc/default/homestack-ups", "UPS_LOW_CHARGE=40\n")
	put("etc/nut/ups.conf", "[ups]\n")
	put("etc/systemd/network/10-bond0.netdev", "[NetDev]\nName=bond0\n")
	put("etc/systemd/network/README", "not a network file")
	put("var/lib/libvirt/qemu/nvram/win11_VARS.fd", "UEFI variables")
	put("etc/docker/daemon.json", `{"log-driver":"json-file"}`)
	put("etc/systemd/system/docker.service.d/wait-for-storage.conf", "[Unit]\nRequiresMountsFor=/mnt/user\n")
	put("etc/systemd/system/libvirtd.service.d/wait-for-storage.conf", "[Unit]\nRequiresMountsFor=/mnt/user\n")
	put("etc/systemd/system/ssh.service.d/override.conf", "not ours")
	os.WriteFile(filepath.Join(a.opt.StateDir, "fans.json"), []byte(`{"curves":[]}`), 0o640)
	os.WriteFile(filepath.Join(a.opt.StateDir, "smb-users.json"), []byte(`["andrew"]`), 0o640)
	for k, v := range map[string]string{
		"zpool list -Ho name":                                      "disk1\ndisk2\n",
		"zpool get -Ho value guid disk1":                           "1111\n",
		"zpool get -Ho value guid disk2":                           "2222\n",
		"pdbedit -L":                                               "andrew:1000:Andrew\nnobody:65534:\n",
		"getent passwd andrew":                                     "andrew:x:1000:100:Andrew:/home/andrew:/bin/bash\n",
		"getent passwd nobody":                                     "nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n",
		"getent group 100":                                         "users:x:100:\n",
		"getent group 65534":                                       "nogroup:x:65534:\n",
		"virsh list --all --name":                                  "win11\nubuntu\n",
		"virsh list --all --autostart --name":                      "win11\n",
		"virsh dumpxml --inactive --security-info win11":           win11XML,
		"virsh dumpxml --inactive --security-info ubuntu":          `<domain type='kvm'><name>ubuntu</name></domain>`,
		"findmnt -rno SOURCE --target /":                           "/dev/sda2\n",
		"findmnt -rno SOURCE --target /mnt/user/backups/homestack": "user\n",
		"findmnt -rno SOURCE --target /mnt/user":                   "user\n",
		"snapraid --version":                                       "snapraid v15.0 by Andrea Mazzoleni, https://www.snapraid.it\n",
	} {
		r.out[k] = v
	}
	return a, r
}

func openBackupFile(t *testing.T, path string) (backup.Manifest, map[string]backup.Entry) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := backup.Open(data, testPass)
	if err != nil {
		t.Fatal(err)
	}
	m, files, err := backup.Unpack(plain)
	if err != nil {
		t.Fatal(err)
	}
	return m, files
}

func TestMakeBackup(t *testing.T) {
	a, r := backupHost(t)
	ctx := context.Background()
	if _, err := a.makeBackup(ctx); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("no passphrase: %v", err)
	}
	k, _ := backup.NewKey(testPass)
	a.saveBackupSettings(backupSettings{Dir: backupDir, Keep: 2, Key: &k})
	if fi, _ := os.Stat(a.backupSettingsPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup.json is %v: it holds the key", fi.Mode().Perm())
	}
	// Two older ones, so the oldest goes; one is from a host name that
	// sorts after this one's, which mustn't make the new one look oldest.
	dir := r.path("mnt/user/backups/homestack")
	os.MkdirAll(dir, 0o700)
	for _, n := range []string{"homestack-backup-zzz-old-name-20260101-021500.hsbak", "homestack-backup-lucy-20260102-021500.hsbak"} {
		os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o600)
	}
	f, err := a.backupNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, b := range a.listBackups(backupDir) {
		names = append(names, b.Name)
	}
	if !slices.Equal(names, []string{f.Name, "homestack-backup-lucy-20260102-021500.hsbak"}) {
		t.Fatalf("kept %v", names)
	}
	if fi, _ := os.Stat(filepath.Join(dir, f.Name)); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup file is %v", fi.Mode().Perm())
	}
	// The summary says what's in it (and a size in KB, not "0 MB").
	last := a.jobs.lastResult("backup")
	if last == nil || !last.OK || !strings.Contains(last.Summary, " KB): the array (2 pools), system settings, shares and their users (2 share users)") ||
		!strings.Contains(last.Summary, "UPS, virtual machines (2 VMs), network, Homestack's account and settings.") || strings.Contains(last.Summary, "Couldn't read") {
		t.Errorf("last result %+v", last)
	}

	m, files := openBackupFile(t, filepath.Join(dir, f.Name))
	var ids []string
	for _, p := range m.Parts {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, []string{"storage", "system", "shares", "schedules", "fans", "ups", "vms", "network", "homestack"}) {
		t.Fatalf("parts %v (missing %v)", ids, m.Missing)
	}
	if hs := string(files["homestack/homestack.json"].Data); !strings.Contains(hs, "andrew") || strings.Contains(hs, "secret-session") {
		t.Errorf("homestack.json: %s", hs)
	}
	var pools []poolID
	json.Unmarshal(files["storage/pools.json"].Data, &pools)
	if len(pools) != 2 || pools[0].GUID != "1111" {
		t.Errorf("pools %+v", pools)
	}
	var users []shareUser
	json.Unmarshal(files["shares/users.json"].Data, &users)
	if len(users) != 2 || users[0] != (shareUser{Name: "andrew", UID: 1000, GID: 100, Group: "users", Home: "/home/andrew", Shell: "/bin/bash"}) {
		t.Errorf("users %+v", users)
	}
	if string(files["vms/win11.nvram"].Data) != "UEFI variables" || files["vms/ubuntu.xml"].Data == nil || string(files["vms/autostart.json"].Data) == "" {
		t.Error("VMs not all there")
	}
	if _, ok := files["network/README"]; ok || files["network/10-bond0.netdev"].Data == nil {
		t.Error("network files")
	}
	var bs backupSettings
	json.Unmarshal(files["schedules/backup.json"].Data, &bs)
	if bs.Key != nil || bs.Dir != backupDir {
		t.Errorf("the key went into the backup: %+v", bs)
	}

	// Never onto the boot drive (an array that isn't mounted).
	r.out["findmnt -rno SOURCE --target /mnt/user/backups/homestack"] = "/dev/sda2\n"
	if _, err := a.backupNow(ctx); err == nil || !strings.Contains(err.Error(), "boot drive") {
		t.Fatalf("on the boot drive: %v", err)
	}
	if last := a.jobs.lastResult("backup"); last == nil || last.OK {
		t.Errorf("a failure must be recorded: %+v", last)
	}
}

// Something that couldn't be read is still saved around, but said.
func TestBackupIncomplete(t *testing.T) {
	a, _ := backupHost(t)
	a.opt.DataDir = filepath.Join(t.TempDir(), "gone") // Homestack's settings aren't there
	k, _ := backup.NewKey(testPass)
	a.saveBackupSettings(backupSettings{Dir: backupDir, Keep: 14, Key: &k})
	f, err := a.backupNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	last := a.jobs.lastResult("backup")
	if len(f.Missing) != 1 || slices.Contains(f.Contents, "Homestack's account and settings") || !strings.Contains(last.Summary, "Couldn't read: Homestack's settings") {
		t.Fatalf("file %+v, last %+v", f, last)
	}
	if human(36_352) != "35 KB" || human(10) != "1 KB" || human(5<<20) != "5 MB" {
		t.Errorf("sizes: %s %s %s", human(36_352), human(10), human(5<<20))
	}
}

// The folder doesn't exist until the first backup makes it: where it will
// be made (/mnt/user) is what's checked, not refused for not existing.
func TestBackupFolderNotMadeYet(t *testing.T) {
	a, r := backupHost(t)
	ctx := context.Background()
	if p := a.backupDirProblem(ctx, backupDir); p != "" {
		t.Fatalf("a folder that isn't there yet: %q", p)
	}
	if !slices.Contains(r.ran, "findmnt -rno SOURCE --target /mnt/user") {
		t.Fatalf("asked about %v", r.ran)
	}
	k, _ := backup.NewKey(testPass)
	a.saveBackupSettings(backupSettings{Dir: backupDir, Keep: 14, Key: &k})
	if _, err := a.makeBackup(ctx); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(r.path("mnt/user/backups/homestack")); err != nil || !fi.IsDir() {
		t.Fatal("the first backup didn't make the folder")
	}
	// With the array stopped, /mnt/user is an empty folder on the boot drive.
	r.out["findmnt -rno SOURCE --target /mnt/user"] = "/dev/sda2\n"
	if p := a.backupDirProblem(ctx, "/mnt/user/somewhere/new"); !strings.Contains(p, "boot drive") {
		t.Fatalf("on the boot drive: %q", p)
	}
}

func httptestGet(a *Agent, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	return w
}

// After a reinstall /mnt/user isn't mounted, but the backups on the cache
// and the data disks are found (and only the folder itself is pruned).
func TestBackupsFoundOnTheDisks(t *testing.T) {
	a, r := backupHost(t)
	for _, p := range []string{"mnt/cache/backups/homestack/homestack-backup-lucy-20261010-021500.hsbak", "mnt/disk2/backups/homestack/homestack-backup-lucy-20261001-021500.hsbak",
		"mnt/disk2/backups/homestack/homestack-backup-lucy-20261010-021500.hsbak"} {
		os.MkdirAll(filepath.Dir(r.path(p)), 0o755)
		os.WriteFile(r.path(p), []byte(p), 0o600)
	}
	files := a.backupView(context.Background()).Files
	if len(files) != 2 || files[0].Dir != "/mnt/cache/backups/homestack" || files[1].Dir != "/mnt/disk2/backups/homestack" {
		t.Fatalf("files %+v", files)
	}
	w := httptestGet(a, "/v1/backups/file?name=homestack-backup-lucy-20261001-021500.hsbak")
	if w.Code != 200 || !strings.HasSuffix(w.Body.String(), "20261001-021500.hsbak") {
		t.Fatalf("download: %d %s", w.Code, w.Body)
	}
	if w := httptestGet(a, "/v1/backups/file?name=../../etc/shadow"); w.Code != 404 {
		t.Fatalf("bad name: %d", w.Code)
	}
	k, _ := backup.NewKey(testPass)
	a.saveBackupSettings(backupSettings{Dir: backupDir, Keep: 1, Key: &k})
	if _, err := a.makeBackup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.path("mnt/cache/backups/homestack/homestack-backup-lucy-20261010-021500.hsbak")); err != nil {
		t.Fatal("pruned a backup outside the folder")
	}
}

// Restoring onto a fresh install: the server's parts come back, each file
// to its fixed place, the files they replace kept.
func TestRestoreBackup(t *testing.T) {
	old, _ := backupHost(t)
	k, _ := backup.NewKey(testPass)
	old.saveBackupSettings(backupSettings{Dir: backupDir, Keep: 14, Key: &k})
	f, err := old.makeBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, files := openBackupFile(t, filepath.Join(old.sys(backupDir), f.Name))

	// The new install: its own boot drive in fstab, nothing else.
	a, r, _ := assignHost(t)
	a.opt.NetDir = r.path("etc/systemd/network")
	t.Setenv("HOMESTACK_SMB_CONF", r.path("etc/samba/smb.conf"))
	t.Setenv("HOMESTACK_AGENT_CRON", r.path("etc/cron.d/homestack-alerts"))
	t.Setenv("HOMESTACK_ALERTS_CONF", r.path("etc/default/homestack-alerts"))
	os.WriteFile(r.path("etc/fstab"), []byte("UUID=new-root / ext4 errors=remount-ro 0 1\n"), 0o644)
	os.Remove(r.path("etc/snapraid.conf"))
	os.MkdirAll(r.path("etc/nut"), 0o755)
	r.mounted = map[string]string{}
	for k, v := range map[string]string{
		"zpool import": "", "mount -a": "", "getent group 100": "users:x:100:\n", "useradd": "", "testparm -s": "Loaded services file OK.\n",
		"systemctl": "", "modprobe": "", "chown": "", "virsh list --all --name": "otherdomain\n", "virsh define": "Domain defined\n", "virsh autostart": "",
	} {
		r.out[k] = v
	}
	parts := []string{"storage", "system", "shares", "schedules", "fans", "ups", "vms", "network"}
	res := a.restoreBackup(context.Background(), files, parts)
	for _, x := range res {
		if !x.OK {
			t.Errorf("%s: %s", x.Part, x.Detail)
		}
	}
	if len(res) != len(parts) {
		t.Fatalf("results %+v", res)
	}

	// The array: snapraid.conf, the pools by ID, only /mnt lines in fstab.
	if readText(r.path("etc/snapraid.conf")) != testSnapConf {
		t.Error("snapraid.conf")
	}
	if !slices.Contains(r.ran, "zpool import 1111") || !slices.Contains(r.ran, "zpool import 2222") || !slices.Contains(r.ran, "mount -a") {
		t.Errorf("ran %v", r.ran)
	}
	fstab := readText(r.path("etc/fstab"))
	if !strings.HasPrefix(fstab, "UUID=new-root / ext4") || strings.Contains(fstab, "root-uuid") || !strings.Contains(fstab, "UUID=old-uuid  /mnt/parity1") || !strings.Contains(fstab, "/mnt/user  fuse.mergerfs") {
		t.Errorf("fstab:\n%s", fstab)
	}
	if !strings.Contains(res[0].Warn, "SnapRAID isn't installed. Install 15.0 or newer") {
		t.Errorf("no SnapRAID warning: %q", res[0].Warn)
	}
	if !slices.ContainsFunc(res[0].Saved, func(s string) bool { return strings.HasPrefix(s, "/etc/fstab.homestack-restore-") }) {
		t.Errorf("the old fstab wasn't kept: %v", res[0].Saved)
	}

	// The share user, with the IDs the files on the array are owned by;
	// nobody is this install's own.
	if !slices.Contains(r.ran, "useradd -M -N -g 100 -s /usr/sbin/nologin -d /home/andrew -u 1000 andrew") || slices.ContainsFunc(r.ran, func(s string) bool { return strings.Contains(s, "nobody") && strings.HasPrefix(s, "useradd") }) {
		t.Errorf("users: %v", r.ran)
	}
	if readText(r.path("var/lib/samba/private/passdb.tdb")) != "TDB passwords" || !strings.Contains(readText(r.path("etc/samba/homestack-shares.conf")), "[Media]") {
		t.Error("Samba")
	}
	if readText(r.path("etc/cron.d/homestack-alerts")) != "17 * * * * root space-check\n" || readText(filepath.Join(a.opt.StateDir, "fans.json")) != `{"curves":[]}` {
		t.Error("schedules or fans")
	}
	if a.loadBackupSettings().Dir != backupDir || a.loadBackupSettings().Key != nil {
		t.Error("backup settings: the folder comes back, the key doesn't")
	}

	// VMs: defined (UEFI variables first), autostart kept; one already there is left alone.
	if readText(r.path("var/lib/libvirt/qemu/nvram/win11_VARS.fd")) != "UEFI variables" || !slices.Contains(r.ran, "virsh autostart win11") {
		t.Errorf("VMs: %v", r.ran)
	}
	if n := len(slices.DeleteFunc(slices.Clone(r.ran), func(s string) bool { return !strings.HasPrefix(s, "virsh define") })); n != 2 {
		t.Errorf("defined %d VMs", n)
	}
	if readText(r.path("etc/systemd/network/10-bond0.netdev")) == "" {
		t.Error("network")
	}
	// Docker's settings and the boot-order drop-ins (only those).
	if readText(r.path("etc/docker/daemon.json")) == "" || readText(r.path("etc/systemd/system/libvirtd.service.d/wait-for-storage.conf")) == "" ||
		readText(r.path("etc/systemd/system/ssh.service.d/override.conf")) != "" || !slices.Contains(r.ran, "systemctl daemon-reload") {
		t.Error("system settings")
	}
}

func TestRestoreSafety(t *testing.T) {
	a, r, _ := assignHost(t)
	a.opt.NetDir = r.path("etc/systemd/network")
	t.Setenv("HOMESTACK_SMB_CONF", r.path("etc/samba/smb.conf"))
	g := &fileGuard{Suffix: "t"}
	ctx := context.Background()

	// Names that aren't plain network files, VMs or nvram under libvirt's
	// folder go nowhere.
	files := map[string]backup.Entry{
		"network/../../cron.d/evil.network": {Data: []byte("x")},
		"network/ok.network":                {Data: []byte("[Match]\n"), Mode: 0o644},
		"vms/evil.xml":                      {Data: []byte(`<domain><nvram>/etc/shadow</nvram></domain>`)},
	}
	if _, err := a.restorePart(ctx, "network", files, g); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.path("etc/cron.d/evil.network")); err == nil {
		t.Fatal("wrote outside the network folder")
	}
	if nvramOf(string(files["vms/evil.xml"].Data)) != "" || nvramOf(`<nvram>/var/lib/libvirt/qemu/nvram/../../x</nvram>`) != "" {
		t.Fatal("nvram outside libvirt's folder accepted")
	}

	// A broken smb.conf on a fresh install: refused, and none left behind.
	r.out["testparm -s"] = "!Error loading services."
	if _, err := a.restorePart(ctx, "shares", map[string]backup.Entry{"shares/smb.conf": {Data: []byte("[broken"), Mode: 0o644}}, g); err == nil || !strings.Contains(err.Error(), "testparm") {
		t.Fatalf("broken smb.conf: %v", err)
	}
	if _, err := os.Stat(r.path("etc/samba/smb.conf")); err == nil {
		t.Fatal("the broken smb.conf was left")
	}

	// A snapraid.conf without data disks isn't used.
	if _, err := a.restorePart(ctx, "storage", map[string]backup.Entry{"storage/snapraid.conf": {Data: []byte("parity /x\n")}}, g); err == nil {
		t.Fatal("empty snapraid.conf accepted")
	}
	if readText(r.path("etc/snapraid.conf")) != testSnapConf {
		t.Fatal("snapraid.conf changed")
	}

	// A user ID taken by someone else: made, but without it, and said so.
	r.out["getent passwd 1000"] = "someone:x:1000:1000::/home/someone:/bin/bash\n"
	r.out["getent group 100"] = "users:x:100:\n"
	r.out["useradd"] = ""
	if msg := a.restoreUser(ctx, shareUser{Name: "andrew", UID: 1000, GID: 100, Group: "users"}); !strings.Contains(msg, "not as user 1000") {
		t.Fatalf("taken uid: %q", msg)
	}
}

// A restore warns when the server's SnapRAID is older than the backup's:
// it may not read the content file or snapraid.conf.
func TestSnapraidVersionWarning(t *testing.T) {
	a, r, _ := assignHost(t)
	ctx := context.Background()
	files := map[string]backup.Entry{"storage/snapraid-version": {Data: []byte("snapraid v15.0 by Andrea Mazzoleni, https://www.snapraid.it\n")}}
	for have, want := range map[string]string{
		"snapraid v12.4 by Andrea Mazzoleni\n":  "This server has SnapRAID 12.4, older than the 15.0",
		"snapraid v14.10 by Andrea Mazzoleni\n": "older than the 15.0",
		"snapraid v15.0 by Andrea Mazzoleni\n":  "",
		"snapraid v15.2 by Andrea Mazzoleni\n":  "",
		"snapraid v16.0 by Andrea Mazzoleni\n":  "",
	} {
		r.out["snapraid --version"] = have
		if got := a.snapraidVersionWarning(ctx, files); want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%s: %q", strings.TrimSpace(have), got)
		}
	}
	if a.snapraidVersionWarning(ctx, map[string]backup.Entry{}) != "" {
		t.Error("an older backup, without the version, warns")
	}
}
