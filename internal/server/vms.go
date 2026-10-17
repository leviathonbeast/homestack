package server

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"

	"homestack/internal/libvirt"
	"homestack/internal/sysinfo"
)

// Virtual machines, through libvirt. Everything here is hidden in the UI
// when libvirt isn't available, so hosts without VMs aren't cluttered.

func vmErr(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, libvirt.ErrUnavailable) {
		status = http.StatusServiceUnavailable
	}
	writeErr(w, status, sentence(err.Error()))
}

func (s *Server) vmStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	ver, err := s.vms.Status(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "error": sentence(err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "version": ver})
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	list, err := s.vms.List(ctx)
	if err != nil {
		vmErr(w, err)
		return
	}
	s.vmstats.want()
	for i := range list {
		s.vmstats.fill(&list[i])
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	vm, err := s.vms.Get(ctx, r.PathValue("name"))
	if err != nil {
		if strings.Contains(err.Error(), "no VM called") {
			writeErr(w, http.StatusNotFound, sentence(err.Error()))
			return
		}
		vmErr(w, err)
		return
	}
	s.vmstats.want()
	s.vmstats.fill(vm)
	writeJSON(w, http.StatusOK, vm)
}

var vmActions = map[string]bool{"start": true, "shutdown": true, "stop": true, "reboot": true, "pause": true, "resume": true, "reset": true}

func (s *Server) vmAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !vmActions[action] {
		writeErr(w, http.StatusBadRequest, "Unknown action.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	s.markVMIntent(r.PathValue("name")) // don't alert about stops the admin asked for
	if err := s.vms.Action(ctx, r.PathValue("name"), action); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) vmAutostart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.vms.SetAutostart(r.Context(), r.PathValue("name"), req.On); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getVMXML(w http.ResponseWriter, r *http.Request) {
	x, err := s.vms.DumpXML(r.Context(), r.PathValue("name"), true)
	if err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"xml": x})
}

func (s *Server) putVMXML(w http.ResponseWriter, r *http.Request) {
	var req struct {
		XML string `json:"xml"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var d libvirt.DomainXML
	if err := xml.Unmarshal([]byte(req.XML), &d); err != nil {
		writeErr(w, http.StatusBadRequest, "That isn't valid XML: "+err.Error())
		return
	}
	if d.Name != r.PathValue("name") {
		writeErr(w, http.StatusBadRequest, "The <name> in the XML must stay the same. To rename a VM, change it here and in the name field of a new VM instead.")
		return
	}
	var disks []string
	for _, dk := range d.Devices.Disks {
		if dk.Device == "disk" || dk.Device == "" {
			disks = append(disks, dk.Source.File)
		}
	}
	if err := s.templateDiskGuard(disks...); err != nil {
		writeErr(w, http.StatusBadRequest, sentence(err.Error()))
		return
	}
	if err := s.vms.Define(r.Context(), req.XML); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// getVMConfig returns a VM's settings for the form (the saved definition,
// which is what the next start uses).
func (s *Server) getVMConfig(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cfg, err := s.vms.Config(ctx, r.PathValue("name"))
	if err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) putVMConfig(w http.ResponseWriter, r *http.Request) {
	var cfg libvirt.VMConfig
	if err := readJSON(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if cfg.Name != r.PathValue("name") {
		writeErr(w, http.StatusBadRequest, "The VM's name can't be changed here.")
		return
	}
	var disks []string
	for _, d := range cfg.Disks {
		if d.Device == "disk" && !d.Remove {
			disks = append(disks, d.Source)
		}
	}
	if err := s.templateDiskGuard(disks...); err != nil {
		writeErr(w, http.StatusBadRequest, sentence(err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := s.vms.SaveConfig(ctx, cfg.Name, cfg, s.store.Settings().VMDomainsDir); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.vms.Undefine(ctx, r.PathValue("name"), r.URL.Query().Get("disks") == "1"); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var req createVMReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Disk.Folder == "" {
		req.Disk.Folder = s.store.Settings().VMDomainsDir
	}
	if err := s.vmNameGuard(req.Name, req.Disk.Folder); err != nil {
		writeErr(w, http.StatusBadRequest, sentence(err.Error()))
		return
	}
	if req.Template != "" {
		s.createFromTemplate(w, r, req)
		return
	}
	if req.Disk.Mode == "existing" {
		if err := s.templateDiskGuard(req.Disk.Path); err != nil {
			writeErr(w, http.StatusBadRequest, sentence(err.Error()))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	x, err := s.vms.Create(ctx, req.NewVM)
	if err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": req.Name, "xml": x})
}

type isoFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// vmOptions lists what the "New VM" form can offer.
func (s *Server) vmOptions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	caps, err := s.vms.Capabilities(ctx)
	if err != nil {
		vmErr(w, err)
		return
	}
	set := s.store.Settings()
	mem := sysinfo.Read(nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"hostCPUs":   runtime.NumCPU(),
		"hostMemory": mem.MemTotal,
		"caps":       caps,
		"bridges":    hostBridges(),
		"networks":   s.vms.Networks(ctx),
		"usb":        s.vms.HostUSB(ctx),
		"isos":       findISOs(set.VMIsosDir),
		"isosDir":    set.VMIsosDir,
		"domainsDir": set.VMDomainsDir,
	})
}

// hostBridges lists Linux bridges, which VMs can join to sit on your LAN.
func hostBridges() []string {
	out := []string{}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, "docker") || strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "virbr") {
			continue
		}
		if st, err := os.Stat("/sys/class/net/" + i.Name + "/bridge"); err == nil && st.IsDir() {
			out = append(out, i.Name)
		}
	}
	sort.Strings(out)
	return out
}

func findISOs(root string) []isoFile {
	out := []isoFile{}
	if root == "" {
		return out
	}
	root = filepath.Clean(root)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.Count(strings.TrimPrefix(p, root), "/") > 3 {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".iso") {
			info, _ := d.Info()
			var size int64
			if info != nil {
				size = info.Size()
			}
			out = append(out, isoFile{Path: p, Name: strings.TrimPrefix(p, root+"/"), Size: size})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// vmConsole bridges noVNC in the browser to the VM's VNC server. Homestack
// runs on the host network, so VNC can stay bound to 127.0.0.1.
func (s *Server) vmConsole(w http.ResponseWriter, r *http.Request) {
	addr, _, err := s.vms.ConsoleAddr(r.Context(), r.PathValue("name"))
	if err != nil {
		vmErr(w, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var d net.Dialer
	tcp, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "can't reach the VM console")
		return
	}
	defer tcp.Close()

	go func() { // VM -> browser
		defer cancel()
		buf := make([]byte, 64<<10)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { // browser -> VM
		defer cancel()
		for {
			_, rd, err := conn.Reader(ctx)
			if err != nil {
				return
			}
			if _, err := io.Copy(tcp, rd); err != nil {
				return
			}
		}
	}()
	<-ctx.Done()
	conn.Close(websocket.StatusNormalClosure, "")
}
