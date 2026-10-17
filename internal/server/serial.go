package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// virsh says this whenever it runs on a terminal without polkit, though the
// connection works; it would only confuse.
var rePolkitNoise = regexp.MustCompile(`(?m)^Authorization not available\. Check if polkit service is running[^\n]*\n`)

// vmSerial is the Serial tab: a VM's serial console in the browser, which
// keeps working when its screen doesn't. Homestack runs `virsh console` on a
// pseudo-terminal and relays it over the signed-in WebSocket, like the
// container console. libvirt allows one session per VM; ?force=1 takes over
// an open one, and only when the person asks to.
//
// Binary messages carry terminal bytes both ways; text messages from the
// browser are JSON, e.g. {"type":"resize","cols":80,"rows":24}. The close
// reason says how it ended: busy, off, noserial, error or ended.
func (s *Server) vmSerial(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	q := r.URL.Query()
	force := q.Get("force") == "1"
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))

	// Same-origin check is on by default in websocket.Accept.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	say := func(text string) {
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("\r\n\x1b[33m"+text+"\x1b[0m\r\n"))
	}
	fail := func(reason, text string) {
		say(text)
		conn.Close(websocket.StatusNormalClosure, reason)
	}

	vm, err := s.vms.Get(ctx, name)
	switch {
	case err != nil:
		fail("error", sentence(err.Error()))
		return
	case vm.State != "running" && vm.State != "paused":
		fail("off", "The VM isn't running.")
		return
	case !vm.Serial:
		fail("noserial", "This VM has no serial port.")
		return
	}

	master, tty, err := openPTY()
	if err != nil {
		fail("error", "Couldn't open a terminal: "+err.Error())
		return
	}
	defer master.Close()
	_ = setPTYSize(master, cols, rows)

	cmd := s.vms.SerialCommand(ctx, name, force)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		tty.Close()
		fail("error", "Couldn't start virsh: "+err.Error())
		return
	}
	tty.Close() // virsh has its own copy; reads end when it exits
	took := ""
	if force {
		took = ", taking it over"
	}
	log.Printf("serial console: %s opened from %s%s", name, s.clientIP(r), took)

	// VM -> browser. The tail of what virsh printed tells "someone else has
	// the console open" apart from an ordinary end.
	var (
		tailMu sync.Mutex
		tail   []byte
		out    sync.WaitGroup
	)
	out.Go(func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				data := rePolkitNoise.ReplaceAll(buf[:n], nil)
				tailMu.Lock()
				tail = append(tail, data...)
				if len(tail) > 1024 {
					tail = tail[len(tail)-1024:]
				}
				tailMu.Unlock()
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				werr := conn.Write(wctx, websocket.MessageBinary, data)
				wcancel()
				if werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	})

	// Browser -> VM
	go func() {
		defer cancel() // the browser left: stop virsh
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				var msg struct {
					Type string `json:"type"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
					_ = setPTYSize(master, msg.Cols, msg.Rows)
				}
				continue
			}
			if _, err := master.Write(data); err != nil {
				return
			}
		}
	}()

	_ = cmd.Wait() // virsh ended: Ctrl+], the VM stopped, taken over, or the browser left
	out.Wait()
	tailMu.Lock()
	said := string(tail)
	tailMu.Unlock()
	reason := "ended"
	switch {
	case strings.Contains(said, "Active console session"):
		reason = "busy"
	case strings.Contains(said, "not running"):
		reason = "off"
	}
	conn.Close(websocket.StatusNormalClosure, reason)
}
