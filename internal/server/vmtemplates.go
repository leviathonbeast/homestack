package server

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"homestack/internal/cloudimg"
	"homestack/internal/cloudinit"
	"homestack/internal/libvirt"
	"homestack/internal/state"
)

// VM templates, like Proxmox's: a disk image to clone new VMs from in
// seconds, instead of installing from an ISO each time. Templates come from
// official cloud images (checked against the publisher's checksums), any
// https link with a checksum, or a VM that's been prepared for it.
//
// Guards, since a template is shared by every VM made from it:
//   - a template's image is never attached to a VM as a writable disk;
//   - a template can't be deleted while linked clones still read from it;
//   - downloads must match a checksum, and must be a plain disk image;
//   - images that refer to another file (a qcow2 backing file) are refused,
//     since that file could be anything on the host.

var errTemplatesDir = errors.New("templates folder clash")

// tplProgress is a template being made, as the list shows it.
type tplProgress struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phase string `json:"phase"` // checksum, downloading, copying, saving
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

// tplBusy tracks template work in progress, for the list and to stop the
// same thing running twice. The download updates it from another goroutine.
type tplBusy struct {
	mu  sync.Mutex
	p   tplProgress
	key string // what it's made from
}

func (b *tplBusy) set(fn func(p *tplProgress)) {
	b.mu.Lock()
	fn(&b.p)
	b.mu.Unlock()
}

func (b *tplBusy) get() tplProgress {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.p
}

type tplState struct {
	mu     sync.RWMutex // clones read-lock it; deleting a template write-locks it
	busyMu sync.Mutex
	busy   map[string]*tplBusy // by template id
}

func (s *Server) busyTemplates() []*tplBusy {
	s.tpl.busyMu.Lock()
	defer s.tpl.busyMu.Unlock()
	out := make([]*tplBusy, 0, len(s.tpl.busy))
	for _, b := range s.tpl.busy {
		out = append(out, b)
	}
	return out
}

type tplView struct {
	state.VMTemplate
	Format  string   `json:"format"`
	Size    uint64   `json:"size"` // virtual size
	Used    uint64   `json:"used"` // on disk
	Missing bool     `json:"missing,omitempty"`
	VMs     []string `json:"vms"` // made from it (from the VMs' own notes)
}

func (s *Server) templates() []state.VMTemplate {
	var out []state.VMTemplate
	s.store.Read(func(st *state.State) { out = slices.Clone(st.VMTemplates) })
	return out
}

func (s *Server) template(id string) (state.VMTemplate, bool) {
	for _, t := range s.templates() {
		if t.ID == id {
			return t, true
		}
	}
	return state.VMTemplate{}, false
}

func (s *Server) listVMTemplates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list := s.templates()
	views := make([]tplView, 0, len(list))
	byDir := map[string][]string{}
	for _, t := range list {
		dir, file := filepath.Split(t.Path)
		byDir[dir] = append(byDir[dir], file)
	}
	infos := map[string]libvirt.VolInfo{}
	for dir, files := range byDir {
		got, err := s.vms.VolumesIn(ctx, dir, files)
		if err != nil {
			vmErr(w, err)
			return
		}
		for f, v := range got {
			infos[filepath.Join(dir, f)] = v
		}
	}
	made := map[string][]string{}
	if vms, err := s.vms.List(ctx); err == nil {
		for _, vm := range vms {
			if vm.Template != "" {
				made[vm.Template] = append(made[vm.Template], vm.Name)
			}
		}
	}
	for _, t := range list {
		v := tplView{VMTemplate: t, VMs: made[t.ID]}
		if v.VMs == nil {
			v.VMs = []string{}
		}
		if info, ok := infos[t.Path]; ok {
			v.Format, v.Size, v.Used = info.Format, info.Capacity, info.Allocation
		} else {
			v.Missing = true
		}
		views = append(views, v)
	}
	busy := []tplProgress{}
	for _, b := range s.busyTemplates() {
		busy = append(busy, b.get())
	}
	var defaults state.CloudInitDefaults
	s.store.Read(func(st *state.State) { defaults = st.CloudInit })
	writeJSON(w, http.StatusOK, map[string]any{
		"templates": views,
		"busy":      busy,
		"catalog":   cloudimg.Catalog,
		"dir":       s.store.Settings().VMTemplatesDir,
		"defaults":  defaults,
	})
}

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

// claimTemplate picks a file-safe id from the name, unique among templates,
// and marks the work as running. It refuses a second run of the same thing.
func (s *Server) claimTemplate(name, key string) (*tplBusy, error) {
	base := strings.Trim(reSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	if base == "" {
		base = "template"
	}
	taken := map[string]bool{}
	for _, t := range s.templates() {
		taken[t.ID] = true
	}
	s.tpl.busyMu.Lock()
	defer s.tpl.busyMu.Unlock()
	for id, b := range s.tpl.busy {
		if b.key == key {
			return nil, errors.New("that template is already being made")
		}
		taken[id] = true
	}
	id := base
	for i := 2; taken[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	if s.tpl.busy == nil {
		s.tpl.busy = map[string]*tplBusy{}
	}
	b := &tplBusy{p: tplProgress{ID: id, Name: name, Phase: "starting"}, key: key}
	s.tpl.busy[id] = b
	return b, nil
}

func (s *Server) releaseTemplate(id string) {
	s.tpl.busyMu.Lock()
	delete(s.tpl.busy, id)
	s.tpl.busyMu.Unlock()
}

type addTemplateReq struct {
	Kind        string `json:"kind"` // catalog | url | vm
	Image       string `json:"image"`
	URL         string `json:"url"`
	Checksum    string `json:"checksum"` // a hash, or a link to a checksum list
	Name        string `json:"name"`
	Description string `json:"description"`
	VM          string `json:"vm"`
	CloudInit   bool   `json:"cloudInit"`
	Prepared    bool   `json:"prepared"` // the VM was cleaned up for cloning
}

// addVMTemplate makes a template, streaming progress like an image pull.
// Everything that can be checked up front is, so mistakes come back as
// plain errors before anything starts.
func (s *Server) addVMTemplate(w http.ResponseWriter, r *http.Request) {
	var req addTemplateReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	if len(req.Name) > 80 || len(req.Description) > 500 {
		writeErr(w, http.StatusBadRequest, "Keep the name under 80 characters and the description under 500.")
		return
	}
	t := state.VMTemplate{Name: req.Name, Description: req.Description, OS: "linux", Firmware: "uefi", CloudInit: true}
	var job func(ctx context.Context, b *tplBusy, out *ndjson) error
	key := ""
	switch req.Kind {
	case "catalog":
		img, ok := cloudimg.Find(req.Image)
		if !ok {
			writeErr(w, http.StatusBadRequest, "Unknown image.")
			return
		}
		t.Name = firstNonEmptyStr(t.Name, img.Name)
		t.Source = img.Name + " cloud image"
		key = img.URL
		job = func(ctx context.Context, b *tplBusy, out *ndjson) error {
			return s.downloadTemplate(ctx, b, out, &t, img.URL, img.SumsURL, "")
		}
	case "url":
		u, err := cloudimg.CheckURL(req.URL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, sentence(err.Error()))
			return
		}
		sum, sumsURL := strings.TrimSpace(req.Checksum), ""
		if strings.HasPrefix(sum, "https://") || strings.HasPrefix(sum, "http://") {
			sum, sumsURL = "", strings.TrimSpace(req.Checksum)
			if _, err := cloudimg.CheckURL(sumsURL); err != nil {
				writeErr(w, http.StatusBadRequest, "Checksum list: "+sentence(err.Error()))
				return
			}
		} else if _, err := cloudimg.NewHash(sum); err != nil {
			writeErr(w, http.StatusBadRequest, "Enter the image's SHA-256 or SHA-512 checksum, or a link to its checksum list. Homestack won't use an image it can't verify.")
			return
		}
		t.Name = firstNonEmptyStr(t.Name, cloudimg.FileName(u))
		t.Source = u.String()
		t.CloudInit = req.CloudInit
		key = u.String()
		job = func(ctx context.Context, b *tplBusy, out *ndjson) error {
			return s.downloadTemplate(ctx, b, out, &t, u.String(), sumsURL, sum)
		}
	case "vm":
		vm, err := s.vms.Get(r.Context(), req.VM)
		if err != nil {
			vmErr(w, err)
			return
		}
		if vm.State != "shutoff" {
			writeErr(w, http.StatusBadRequest, "Shut "+vm.Name+" down first, so its disk isn't changing while it's copied.")
			return
		}
		if !req.Prepared {
			writeErr(w, http.StatusBadRequest, "Confirm the VM has been prepared for cloning first.")
			return
		}
		var disk *libvirt.Disk
		for i, d := range vm.Disks {
			if d.Device == "disk" {
				disk = &vm.Disks[i]
				break
			}
		}
		if disk == nil || disk.Path == "" || strings.HasPrefix(disk.Path, "/dev/") {
			writeErr(w, http.StatusBadRequest, vm.Name+" has no disk image file to copy (block devices and passed-through disks can't be templates).")
			return
		}
		t.Name = firstNonEmptyStr(t.Name, vm.Name)
		t.Source = "VM " + vm.Name
		t.Firmware = vm.Firmware
		t.OS = osFamily(vm.OS)
		t.CloudInit = req.CloudInit && t.OS == "linux"
		key = "vm:" + vm.Name
		src, format := disk.Path, disk.Format
		job = func(ctx context.Context, b *tplBusy, out *ndjson) error {
			return s.copyVMToTemplate(ctx, b, out, &t, src, format)
		}
	default:
		writeErr(w, http.StatusBadRequest, "Unknown template source.")
		return
	}

	dir := s.store.Settings().VMTemplatesDir
	if !filepath.IsAbs(dir) {
		writeErr(w, http.StatusBadRequest, "Set a full path for the templates folder in Settings → Stacks and VMs.")
		return
	}
	b, err := s.claimTemplate(t.Name, key)
	if err != nil {
		writeErr(w, http.StatusConflict, sentence(err.Error()))
		return
	}
	defer s.releaseTemplate(b.get().ID)
	t.ID = b.get().ID
	t.Path = filepath.Join(dir, t.ID+".qcow2") // downloads of raw images switch to .img

	// The work carries on if the browser goes away; the Templates page shows it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Hour)
	defer cancel()
	out := newNDJSON(w)
	err = job(ctx, b, out)
	if err == nil {
		t.Created = time.Now().UTC()
		err = s.store.Update(func(st *state.State) error {
			st.VMTemplates = append(st.VMTemplates, t)
			return nil
		})
		if err != nil {
			_ = s.vms.DeleteFile(context.Background(), t.Path)
		} else {
			out.line("Template " + t.Name + " is ready. Create VMs from it on the New VM page.")
		}
	}
	out.done(err)
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func osFamily(os string) string {
	o := strings.ToLower(os)
	switch {
	case strings.Contains(o, "win"):
		return "windows"
	case o == "" || o == "other":
		return "other"
	}
	return "linux"
}

// progressReader counts bytes and reports every 10%, or every 20 seconds
// when the size isn't known.
type progressReader struct {
	r      io.Reader
	n      atomic.Int64
	total  int64
	last   time.Time
	step   int64
	report func(done int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	done := p.n.Add(int64(n))
	if p.total > 0 {
		if s := done * 10 / p.total; s > p.step {
			p.step = s
			p.report(done)
		}
	} else if time.Since(p.last) > 20*time.Second {
		p.last = time.Now()
		p.report(done)
	}
	return n, err
}

func (s *Server) downloadTemplate(ctx context.Context, b *tplBusy, out *ndjson, t *state.VMTemplate, url, sumsURL, sum string) error {
	file := url[strings.LastIndex(url, "/")+1:]
	if i := strings.IndexAny(file, "?#"); i >= 0 {
		file = file[:i]
	}
	if sumsURL != "" {
		b.set(func(p *tplProgress) { p.Phase = "checksum" })
		out.line("Fetching the published checksums: " + sumsURL)
		var err error
		if sum, err = cloudimg.ExpectedSum(ctx, sumsURL, file); err != nil {
			return err
		}
	}
	h, err := cloudimg.NewHash(sum)
	if err != nil {
		return err
	}
	sum = strings.ToLower(sum)
	out.line("Expected checksum: " + sum)

	body, size, err := cloudimg.Open(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	br := bufio.NewReaderSize(body, 64<<10)
	head, _ := br.Peek(512)
	kind := cloudimg.Kind(head)
	if kind == "" {
		return errors.New("that file isn't a disk image (qcow2 or raw). Compressed images (.xz, .gz, .tar) aren't supported: use the .qcow2 or .img link")
	}
	if kind == "raw" {
		t.Path = strings.TrimSuffix(t.Path, ".qcow2") + ".img"
	}
	b.set(func(p *tplProgress) { p.Phase, p.Total = "downloading", size })
	sizeText := "size unknown"
	if size > 0 {
		sizeText = humanBytes(uint64(size))
	}
	out.line(fmt.Sprintf("Downloading %s (%s, %s) into %s", file, kind, sizeText, t.Path))
	pr := &progressReader{r: io.TeeReader(br, h), total: size, last: time.Now(), report: func(done int64) {
		b.set(func(p *tplProgress) { p.Done = done })
		if size > 0 {
			out.line(fmt.Sprintf("  %3d%%  %s of %s", done*100/size, humanBytes(uint64(done)), humanBytes(uint64(size))))
		} else {
			out.line("  " + humanBytes(uint64(done)))
		}
	}}
	limited := io.LimitReader(pr, cloudimg.MaxSize+1)
	info, err := s.vms.Upload(ctx, t.Path, limited)
	if err != nil {
		return fmt.Errorf("the download failed: %v", err)
	}
	gotBytes := pr.n.Load()
	discard := func(reason string) error {
		_ = s.vms.DeleteFile(context.Background(), t.Path)
		return errors.New(reason)
	}
	switch {
	case gotBytes > cloudimg.MaxSize:
		return discard("the file is bigger than an image should be; it was deleted")
	case size > 0 && gotBytes != size:
		return discard("the download was cut short; it was deleted. Try again")
	}
	b.set(func(p *tplProgress) { p.Phase, p.Done = "saving", gotBytes })
	got := hex.EncodeToString(h.Sum(nil))
	if got != sum {
		return discard("the download doesn't match its published checksum, so it was deleted. A new build may have been published while it downloaded: try again. If it keeps happening, don't use this image")
	}
	out.line("Checksum matches.")
	if err := checkImage(info, kind); err != nil {
		return discard(err.Error())
	}
	t.Checksum = sum
	return nil
}

// checkImage refuses images that point at other files, or aren't what they seemed.
func checkImage(info libvirt.VolInfo, kind string) error {
	if info.Backing != "" {
		return fmt.Errorf("the image refers to another file (%s) and was deleted: an image that reads other files on the server isn't safe to use", info.Backing)
	}
	if kind != "" && info.Format != kind {
		return fmt.Errorf("libvirt sees the image as %s, not %s; it was deleted", firstNonEmptyStr(info.Format, "unknown"), kind)
	}
	if info.Format != "qcow2" && info.Format != "raw" {
		return fmt.Errorf("unsupported image format %q", info.Format)
	}
	return nil
}

func (s *Server) copyVMToTemplate(ctx context.Context, b *tplBusy, out *ndjson, t *state.VMTemplate, src, declared string) error {
	info, err := s.vms.VolumeInfo(ctx, src)
	if err != nil {
		return fmt.Errorf("couldn't read the VM's disk: %v", err)
	}
	// Guard: the VM's settings and the file must agree on the format. A
	// guest can write anything onto a raw disk, including a header that
	// makes it look like a qcow2 file pointing at a host file.
	if declared == "" {
		declared = "raw"
	}
	if info.Format != declared {
		return fmt.Errorf("the VM uses its disk as %s, but the file looks like %s. Refusing to copy it", declared, firstNonEmptyStr(info.Format, "unknown"))
	}
	if info.Backing != "" {
		ok := false
		for _, other := range s.templates() {
			ok = ok || other.Path == info.Backing
		}
		if !ok {
			return fmt.Errorf("the VM's disk reads from %s, which isn't one of Homestack's templates. Refusing to copy it", info.Backing)
		}
		out.line("It's a linked clone: the copy will include everything from its template too.")
	}
	b.set(func(p *tplProgress) { p.Phase, p.Total = "copying", int64(info.Allocation) })
	out.line(fmt.Sprintf("Copying %s (%s used of %s) to %s. Large disks take a few minutes.", src, humanBytes(info.Allocation), humanBytes(info.Capacity), t.Path))
	got, err := s.vms.CopyToTemplate(ctx, info, t.Path)
	if err != nil {
		return fmt.Errorf("the copy failed: %v", err)
	}
	if err := checkImage(got, "qcow2"); err != nil {
		_ = s.vms.DeleteFile(context.Background(), t.Path)
		return err
	}
	b.set(func(p *tplProgress) { p.Done = p.Total })
	return nil
}

func (s *Server) putVMTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		CloudInit   *bool  `json:"cloudInit"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name, req.Description = strings.TrimSpace(req.Name), strings.TrimSpace(req.Description)
	if req.Name == "" || len(req.Name) > 80 || len(req.Description) > 500 {
		writeErr(w, http.StatusBadRequest, "Give it a name under 80 characters, and a description under 500.")
		return
	}
	id := r.PathValue("id")
	err := s.store.Update(func(st *state.State) error {
		for i := range st.VMTemplates {
			if t := &st.VMTemplates[i]; t.ID == id {
				t.Name, t.Description = req.Name, req.Description
				if req.CloudInit != nil {
					t.CloudInit = *req.CloudInit && t.OS == "linux"
				}
				return nil
			}
		}
		return errors.New("there's no template " + id)
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, sentence(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteVMTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.template(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "There's no such template.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	// No new clones while checking and deleting.
	s.tpl.mu.Lock()
	defer s.tpl.mu.Unlock()
	users, err := s.vms.BackedBy(ctx, t.Path)
	if err != nil {
		vmErr(w, fmt.Errorf("couldn't check which VMs use it, so it was kept: %v", err))
		return
	}
	if len(users) > 0 {
		writeErr(w, http.StatusConflict, fmt.Sprintf("Kept: %s %s from this template, and would stop working without it. Delete %s first.",
			strings.Join(users, ", "), pick(len(users) == 1, "is a linked clone that reads", "are linked clones that read"), pick(len(users) == 1, "that VM", "those VMs")))
		return
	}
	if s.vms.FileExists(ctx, t.Path) {
		if err := s.vms.DeleteFile(ctx, t.Path); err != nil {
			vmErr(w, fmt.Errorf("couldn't delete %s: %v", t.Path, err))
			return
		}
	}
	err = s.store.Update(func(st *state.State) error {
		st.VMTemplates = slices.DeleteFunc(st.VMTemplates, func(x state.VMTemplate) bool { return x.ID == t.ID })
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save the change.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// templateDiskGuard refuses writable disks inside the templates folder or
// on a template's image: a VM writing to it would corrupt every linked
// clone at once.
func (s *Server) templateDiskGuard(paths ...string) error {
	dir := filepath.Clean(s.store.Settings().VMTemplatesDir)
	tpls := s.templates()
	for _, p := range paths {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		for _, t := range tpls {
			if p == filepath.Clean(t.Path) {
				return fmt.Errorf("%s is the disk image of the template %s. VMs can't use it directly, because writing to it would break every VM cloned from it. Create a VM from the template instead", p, t.Name)
			}
		}
		if strings.HasPrefix(p, dir+"/") {
			return fmt.Errorf("%s is in the templates folder (%s). VMs can't use disks there, because templates must never change. Put the disk somewhere else, like %s", p, dir, s.store.Settings().VMDomainsDir)
		}
	}
	return nil
}

// vmNameGuard keeps VM names off Homestack's own page names, and VM folders
// out of the templates folder.
func (s *Server) vmNameGuard(name, folder string) error {
	switch strings.ToLower(name) {
	case "new", "templates", "options", "status":
		return fmt.Errorf("%q is the name of one of Homestack's pages; pick another name", name)
	}
	set := s.store.Settings()
	if filepath.Join(firstNonEmptyStr(folder, set.VMDomainsDir), name) == filepath.Clean(set.VMTemplatesDir) {
		return errors.New("a VM with that name would share the templates folder; pick another name")
	}
	return nil
}

type createVMReq struct {
	libvirt.NewVM
	CloudInit *cloudinit.Config `json:"cloudInit,omitempty"`
}

// createFromTemplate is "New VM" with a template: clone its disk, write the
// cloud-init seed and define the VM.
func (s *Server) createFromTemplate(w http.ResponseWriter, r *http.Request, req createVMReq) {
	t, ok := s.template(req.Template)
	if !ok {
		writeErr(w, http.StatusBadRequest, "That template doesn't exist any more.")
		return
	}
	var (
		seed     []byte
		remember *state.CloudInitDefaults
	)
	if t.CloudInit {
		if req.CloudInit == nil {
			writeErr(w, http.StatusBadRequest, "Fill in the login for the new VM.")
			return
		}
		c := *req.CloudInit
		c.Normalize()
		if c.Hostname == "" {
			c.Hostname = cloudinit.HostnameFrom(req.Name)
		}
		if c.Timezone == "" {
			c.Timezone = os.Getenv("TZ") // the same as Homestack's
		}
		if err := c.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, sentence(err.Error()))
			return
		}
		if strings.TrimSpace(req.Network.Source) != "" {
			req.Network.MAC = libvirt.NewMAC()
		}
		var err error
		if seed, err = cloudinit.Seed(c, req.Network.MAC); err != nil {
			writeErr(w, http.StatusInternalServerError, "Couldn't build the cloud-init disk: "+err.Error())
			return
		}
		remember = &state.CloudInitDefaults{User: c.User, SSHKeys: c.SSHKeys}
	}
	req.OS, req.Firmware = t.OS, t.Firmware
	req.TPM, req.ISO, req.VirtioISO = false, "", ""
	linked := req.Clone != "full"

	timeout := 2 * time.Minute
	if !linked {
		timeout = 60 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	defer cancel()
	s.tpl.mu.RLock() // the template can't be deleted mid-clone
	defer s.tpl.mu.RUnlock()
	info, err := s.vms.VolumeInfo(ctx, t.Path)
	if err != nil {
		vmErr(w, fmt.Errorf("the template's image %s can't be read: %v", t.Path, err))
		return
	}
	if err := checkImage(info, ""); err != nil {
		writeErr(w, http.StatusBadRequest, "The template's image isn't safe to use: "+sentence(err.Error()))
		return
	}
	x, err := s.vms.CreateFromTemplate(ctx, req.NewVM, libvirt.TemplateDisk{ID: t.ID, Image: info, Linked: linked}, seed)
	if err != nil && x == "" {
		vmErr(w, err)
		return
	}
	if remember != nil { // pre-fill the next one
		_ = s.store.Update(func(st *state.State) error {
			st.CloudInit = *remember
			return nil
		})
	}
	if err != nil { // created, but didn't start
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": req.Name, "xml": x})
}
