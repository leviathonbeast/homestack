package server

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"homestack/internal/docker"
	"homestack/internal/notify"
	"homestack/internal/state"
)

// ---------- Settings ----------

func (s *Server) notifyConfig() notify.Config { return s.store.Notifications().NotifyConfig() }

func tokenHint(t string) string {
	if t == "" {
		return ""
	}
	if len(t) > 8 {
		return t[:3] + "…" + t[len(t)-4:]
	}
	return "set"
}

func (s *Server) getNotifications(w http.ResponseWriter, r *http.Request) {
	n := s.store.Notifications()
	cfg := s.notifyConfig()
	type ev struct {
		notify.EventInfo
		Enabled bool `json:"enabled"`
	}
	var events []ev
	for _, e := range notify.Events {
		events = append(events, ev{EventInfo: e, Enabled: cfg.EventEnabled(e.Key)})
	}
	host, _ := os.Hostname()
	server := n.NtfyServer
	if server == "" {
		server = notify.DefaultNtfyServer
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":  n.DiscordWebhook != "",
		"webhookHint": notify.Hint(n.DiscordWebhook),
		"serverName":  n.ServerName,
		"hostname":    host,
		"events":      events,
		"recent":      s.notifier.Recent(),
		"ntfy": map[string]any{
			"configured": n.NtfyTopic != "",
			"server":     server,
			"topic":      n.NtfyTopic,
			"mode":       notify.NtfyMode(n.NtfyMode),
			"tokenHint":  tokenHint(n.NtfyToken),
		},
	})
}

type ntfyReq struct {
	Server string  `json:"server"`
	Topic  string  `json:"topic"` // "" removes ntfy
	Token  *string `json:"token"` // omitted keeps it, "" removes it
	Mode   string  `json:"mode"`
}

func (q *ntfyReq) validate() (server, topic string, err error) {
	if q.Token != nil && len(*q.Token) > 200 {
		return "", "", fmt.Errorf("That access token is too long.")
	}
	return notify.ValidateNtfy(q.Server, q.Topic)
}

func (s *Server) putNotifications(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Webhook    *string         `json:"webhook"` // "" removes it; omitted keeps it
		Events     map[string]bool `json:"events"`
		ServerName *string         `json:"serverName"`
		Ntfy       *ntfyReq        `json:"ntfy"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var webhook string
	if req.Webhook != nil && strings.TrimSpace(*req.Webhook) != "" {
		var err error
		if webhook, err = notify.ValidateWebhook(*req.Webhook); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var ntfyServer, ntfyTopic string
	if req.Ntfy != nil && strings.TrimSpace(req.Ntfy.Topic) != "" {
		var err error
		if ntfyServer, ntfyTopic, err = req.Ntfy.validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	known := map[string]bool{}
	for _, e := range notify.Events {
		known[e.Key] = true
	}
	for k := range req.Events {
		if !known[k] {
			writeErr(w, http.StatusBadRequest, "Unknown notification type: "+k)
			return
		}
	}
	if req.ServerName != nil && len(*req.ServerName) > 100 {
		writeErr(w, http.StatusBadRequest, "Keep the server name under 100 characters.")
		return
	}
	err := s.store.Update(func(st *state.State) error {
		n := &st.Notifications
		if req.Webhook != nil {
			n.DiscordWebhook = webhook
		}
		if req.Ntfy != nil {
			if ntfyTopic == "" {
				n.NtfyServer, n.NtfyTopic, n.NtfyToken, n.NtfyMode = "", "", "", ""
			} else {
				n.NtfyTopic, n.NtfyMode = ntfyTopic, notify.NtfyMode(req.Ntfy.Mode)
				n.NtfyServer = ntfyServer
				if ntfyServer == notify.DefaultNtfyServer {
					n.NtfyServer = ""
				}
				if req.Ntfy.Token != nil {
					n.NtfyToken = strings.TrimSpace(*req.Ntfy.Token)
				}
			}
		}
		if req.Events != nil {
			if n.Events == nil {
				n.Events = map[string]bool{}
			}
			maps.Copy(n.Events, req.Events)
		}
		if req.ServerName != nil {
			n.ServerName = strings.TrimSpace(*req.ServerName)
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save settings.")
		return
	}
	s.getNotifications(w, r)
}

// testNotification sends a message straight away and reports the answer. It
// can test a Discord URL or ntfy topic before they're saved.
func (s *Server) testNotification(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target  string   `json:"target"` // discord (default) or ntfy
		Webhook string   `json:"webhook"`
		Ntfy    *ntfyReq `json:"ntfy"`
	}
	_ = readJSON(r, &req)
	cfg := s.notifyConfig()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if req.Target == "ntfy" {
		n := cfg.Ntfy
		if req.Ntfy != nil && strings.TrimSpace(req.Ntfy.Topic) != "" {
			server, topic, err := req.Ntfy.validate()
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			n.Server, n.Topic = server, topic
			if req.Ntfy.Token != nil {
				n.Token = strings.TrimSpace(*req.Ntfy.Token)
			}
		}
		if !n.Enabled() {
			writeErr(w, http.StatusBadRequest, "Enter an ntfy topic first.")
			return
		}
		m := notify.Message{Event: "test", Level: notify.Success, Title: "Homestack backup notifications are working",
			Body: "If Discord can't be reached, alerts come here instead."}
		err := notify.SendNtfy(ctx, n, m, cfg.ServerName, "")
		s.notifier.RecordVia(m, "ntfy", err)
		if err != nil {
			writeErr(w, http.StatusBadGateway, sentence(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	webhook := cfg.Webhook
	if strings.TrimSpace(req.Webhook) != "" {
		var err error
		if webhook, err = notify.ValidateWebhook(req.Webhook); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if webhook == "" {
		writeErr(w, http.StatusBadRequest, "Paste a Discord webhook URL first.")
		return
	}
	m := notify.Message{
		Event: "test", Level: notify.Success, Title: "Homestack notifications are working",
		Body: "This channel will get the alerts you switched on in **Settings → Notifications**.",
	}
	err := notify.Send(ctx, webhook, m, cfg.ServerName)
	s.notifier.RecordVia(m, "discord", err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, sentence(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- Homestack started ----------

func (s *Server) announceStart(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
	body := fmt.Sprintf("Version %s is up.", s.cfg.Version)
	level := notify.Info
	if up := hostUptime(); up > 0 && up < 15*time.Minute {
		body = fmt.Sprintf("The server started %s ago. Version %s is up.", roughDuration(up), s.cfg.Version)
		level = notify.Warning
	}
	s.notifier.Notify(notify.Message{Event: "system.started", Level: level, Title: "Homestack started", Body: body})
}

func hostUptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}

func roughDuration(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1f hours", d.Hours())
	}
}

// ---------- Containers ----------

// containerWatch turns the Docker events stream into alerts. It tells crashes
// apart from deliberate stops: `docker stop`, compose down/up, Homestack's own
// buttons and a shutting-down daemon all send a "kill" first, a crash doesn't.
type containerWatch struct {
	mu        sync.Mutex
	killed    map[string]time.Time   // container id -> last kill
	dies      map[string][]time.Time // container name -> recent crash times
	unhealthy map[string]bool        // container name -> we reported it unhealthy
}

func newContainerWatch() *containerWatch {
	return &containerWatch{killed: map[string]time.Time{}, dies: map[string][]time.Time{}, unhealthy: map[string]bool{}}
}

func (s *Server) watchContainers(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.docker.Events(ctx, s.cwatch.handle(s.notifier))
		if ctx.Err() != nil {
			return
		}
		log.Printf("notifications: docker events stream ended (%v), reconnecting", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

const loopWindow = 10 * time.Minute

func containerLabel(ev docker.Event) (name, fields string) {
	a := ev.Actor.Attributes
	name = a["name"]
	if name == "" && len(ev.Actor.ID) >= 12 {
		name = ev.Actor.ID[:12]
	}
	return name, a[docker.LabelProject]
}

func (cw *containerWatch) handle(n *notify.Notifier) func(docker.Event) {
	return func(ev docker.Event) {
		if ev.Type != "container" {
			return
		}
		name, project := containerLabel(ev)
		img := ev.Actor.Attributes["image"]
		fields := []notify.Field{{Name: "Container", Value: "`" + name + "`", Inline: true}}
		if project != "" {
			fields = append(fields, notify.Field{Name: "Stack", Value: project, Inline: true})
		}
		if img != "" {
			fields = append(fields, notify.Field{Name: "Image", Value: "`" + img + "`", Inline: true})
		}
		now := time.Now()

		switch {
		case ev.Action == "kill":
			cw.mu.Lock()
			cw.killed[ev.Actor.ID] = now
			cw.mu.Unlock()

		case ev.Action == "oom":
			n.Notify(notify.Message{
				Event: "container.oom", Level: notify.Error, Title: "Out of memory: " + name,
				Body:   "The kernel killed this container because it ran out of memory. Raise its memory limit, or look for a leak.",
				Fields: fields, Key: "oom:" + name, Cooldown: 30 * time.Minute,
			})

		case ev.Action == "die":
			code := ev.Actor.Attributes["exitCode"]
			cw.mu.Lock()
			k, wasKilled := cw.killed[ev.Actor.ID]
			delete(cw.killed, ev.Actor.ID)
			// Tidy old kill markers now and then.
			for id, t := range cw.killed {
				if now.Sub(t) > time.Hour {
					delete(cw.killed, id)
				}
			}
			if code == "0" || (wasKilled && now.Sub(k) < 2*time.Minute) {
				cw.mu.Unlock()
				return
			}
			recent := []time.Time{now}
			for _, t := range cw.dies[name] {
				if now.Sub(t) < loopWindow {
					recent = append(recent, t)
				}
			}
			cw.dies[name] = recent
			cw.mu.Unlock()

			f := append(fields, notify.Field{Name: "Exit code", Value: code, Inline: true})
			if len(recent) >= 3 {
				n.Notify(notify.Message{
					Event: "container.restartloop", Level: notify.Error, Title: "Keeps restarting: " + name,
					Body:   fmt.Sprintf("It has crashed %d times in the last 10 minutes. Check its logs in Homestack.", len(recent)),
					Fields: f, Key: "loop:" + name, Cooldown: time.Hour,
				})
				return
			}
			n.Notify(notify.Message{
				Event: "container.crash", Level: notify.Error, Title: "Crashed: " + name,
				Body:   "The container stopped with an error. " + exitHint(code),
				Fields: f, Key: "crash:" + name, Cooldown: 15 * time.Minute,
			})

		case strings.HasPrefix(ev.Action, "health_status"):
			status := strings.TrimSpace(strings.TrimPrefix(ev.Action, "health_status:"))
			cw.mu.Lock()
			was := cw.unhealthy[name]
			switch status {
			case "unhealthy":
				cw.unhealthy[name] = true
			case "healthy":
				delete(cw.unhealthy, name)
			}
			cw.mu.Unlock()
			if status == "unhealthy" && !was {
				n.Notify(notify.Message{
					Event: "container.unhealthy", Level: notify.Warning, Title: "Unhealthy: " + name,
					Body:   "Its health check is failing. It's still running, but may not be working properly.",
					Fields: fields, Key: "unhealthy:" + name, Cooldown: time.Hour,
				})
			} else if status == "healthy" && was {
				n.Forget("unhealthy:" + name)
				n.Notify(notify.Message{
					Event: "container.unhealthy", Level: notify.Success, Title: "Healthy again: " + name,
					Body: "Its health check is passing again.", Fields: fields,
				})
			}

		case ev.Action == "destroy":
			cw.mu.Lock()
			delete(cw.unhealthy, name)
			cw.mu.Unlock()
		}
	}
}

func exitHint(code string) string {
	switch code {
	case "137":
		return "Exit code 137 means it was killed, often for running out of memory."
	case "139":
		return "Exit code 139 means it crashed with a segmentation fault."
	case "143":
		return "Exit code 143 means it was told to stop."
	case "1", "2":
		return "Its logs should say why."
	}
	return ""
}

// ---------- Stacks ----------

func (s *Server) notifyStackResult(name, action string, err error, tail []string) {
	if err == nil {
		if action == "update" {
			s.notifier.Notify(notify.Message{
				Event: "stack.updated", Level: notify.Success, Title: "Updated: " + name,
				Body: "Pulled the latest images and recreated any containers that changed.",
			})
		}
		return
	}
	body := sentence(err.Error())
	if len(tail) > 0 {
		body += "\n```\n" + strings.Join(tail, "\n") + "\n```"
	}
	s.notifier.Notify(notify.Message{
		Event: "stack.failed", Level: notify.Error, Title: fmt.Sprintf("Stack %s failed: %s", action, name),
		Body: body, Fields: []notify.Field{{Name: "Stack", Value: name, Inline: true}, {Name: "Action", Value: action, Inline: true}},
	})
}

// tailBuffer keeps the last few lines of a command's output for alerts.
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (t *tailBuffer) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	line = strings.TrimRight(line, "\r\n")
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *tailBuffer) get() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.lines...)
}

// ---------- Image updates ----------

// notifyNewUpdates reports images that have gained an update since the last
// report. Images that are up to date again are forgotten, so their next
// update is reported too.
func (s *Server) notifyNewUpdates(results map[string]updateResult) {
	s.updNotifiedMu.Lock()
	var fresh []string
	for ref, r := range results {
		if r.Available && !s.updNotified[ref] {
			s.updNotified[ref] = true
			fresh = append(fresh, r.Image)
		}
	}
	for ref := range s.updNotified {
		if r, ok := results[ref]; !ok || !r.Available {
			delete(s.updNotified, ref)
		}
	}
	s.updNotifiedMu.Unlock()
	if len(fresh) == 0 {
		return
	}
	sort.Strings(fresh)
	shown := fresh
	if len(shown) > 15 {
		shown = shown[:15]
	}
	var b strings.Builder
	for _, img := range shown {
		b.WriteString("• `" + img + "`\n")
	}
	if extra := len(fresh) - len(shown); extra > 0 {
		fmt.Fprintf(&b, "…and %d more\n", extra)
	}
	b.WriteString("\nOpen Homestack and click **Update** on each stack when you're ready.")
	title := "1 image update available"
	if len(fresh) > 1 {
		title = fmt.Sprintf("%d image updates available", len(fresh))
	}
	s.notifier.Notify(notify.Message{Event: "updates.available", Level: notify.Info, Title: title, Body: b.String()})
}

// ---------- VMs ----------

func (s *Server) markVMIntent(name string) {
	s.vmIntentMu.Lock()
	s.vmIntent[name] = time.Now()
	s.vmIntentMu.Unlock()
}

func (s *Server) vmIntended(name string) bool {
	s.vmIntentMu.Lock()
	defer s.vmIntentMu.Unlock()
	t, ok := s.vmIntent[name]
	return ok && time.Since(t) < 5*time.Minute
}

// watchVMs polls libvirt every 30 seconds and reports state changes. It stays
// idle on hosts without libvirt.
func (s *Server) watchVMs(ctx context.Context) {
	var prev map[string]string
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		cfg := s.notifyConfig()
		if cfg.Webhook == "" || (!cfg.EventEnabled("vm.stopped") && !cfg.EventEnabled("vm.started")) {
			prev = nil
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		list, err := s.vms.List(lctx)
		cancel()
		if err != nil {
			continue
		}
		cur := map[string]string{}
		for _, vm := range list {
			cur[vm.Name] = vm.State
		}
		if prev != nil {
			for name, st := range cur {
				was, known := prev[name]
				if !known || was == st {
					continue
				}
				s.vmChanged(name, was, st)
			}
		}
		prev = cur
	}
}

func (s *Server) vmChanged(name, was, now string) {
	field := []notify.Field{{Name: "VM", Value: name, Inline: true}, {Name: "Was", Value: was, Inline: true}}
	switch {
	case now == "crashed":
		s.notifier.Notify(notify.Message{Event: "vm.stopped", Level: notify.Error, Title: "VM crashed: " + name,
			Body: "The VM stopped unexpectedly. Check its console and the libvirt log (`/var/log/libvirt/qemu/" + name + ".log`).", Fields: field})
	case now == "shutoff" && was != "shutoff":
		if s.vmIntended(name) {
			return
		}
		s.notifier.Notify(notify.Message{Event: "vm.stopped", Level: notify.Warning, Title: "VM stopped: " + name,
			Body: "It was shut down from inside the VM or by the host.", Fields: field})
	case now == "running" && (was == "shutoff" || was == "crashed"):
		if s.vmIntended(name) {
			return
		}
		s.notifier.Notify(notify.Message{Event: "vm.started", Level: notify.Info, Title: "VM started: " + name, Fields: field})
	}
}
