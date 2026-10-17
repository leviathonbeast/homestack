// Package notify sends Homestack's alerts to a Discord channel through a
// webhook. A webhook only lets Homestack post messages: nothing listens for
// commands, so it adds no way in to the server.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level sets the colour and icon of a message.
type Level string

const (
	Info    Level = "info"
	Success Level = "success"
	Warning Level = "warning"
	Error   Level = "error"
)

// ParseLevel accepts the level names used by the CLI and scripts.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "ok", "good":
		return Success
	case "warning", "warn":
		return Warning
	case "error", "err", "critical", "crit":
		return Error
	default:
		return Info
	}
}

var levelColor = map[Level]int{
	Info:    0x3b82f6, // blue
	Success: 0x22c55e, // green
	Warning: 0xf59e0b, // amber
	Error:   0xef4444, // red
}

var levelIcon = map[Level]string{Info: "ℹ️", Success: "✅", Warning: "⚠️", Error: "🛑"}

// Field is a small labelled value shown under the message.
type Field struct {
	Name   string
	Value  string
	Inline bool
}

// Message is one notification.
type Message struct {
	Event    string // key from Events, used for the on/off switches
	Level    Level
	Title    string
	Body     string
	Fields   []Field
	Key      string        // messages with the same key are sent at most once per Cooldown
	Cooldown time.Duration // 0 = no de-duplication
}

// EventInfo describes one kind of notification for the settings page.
type EventInfo struct {
	Key         string `json:"key"`
	Group       string `json:"group"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
}

// Events is every kind of notification Homestack can send.
var Events = []EventInfo{
	{"container.crash", "Containers", "Crashed", "A container stops with an error when nobody asked it to.", true},
	{"container.restartloop", "Containers", "Keeps restarting", "A container crashes 3 or more times within 10 minutes.", true},
	{"container.oom", "Containers", "Out of memory", "The kernel kills a container for using too much memory.", true},
	{"container.unhealthy", "Containers", "Health check failing", "A container's health check reports it unhealthy, and when it recovers.", true},
	{"stack.failed", "Stacks", "Deploy or update failed", "A stack action (deploy, update, pull…) ends with an error.", true},
	{"stack.updated", "Stacks", "Update finished", "A stack update completes successfully.", false},
	{"updates.available", "Stacks", "Image updates found", "The update check finds new versions of images you use.", true},
	{"vm.stopped", "Virtual machines", "Stopped or crashed", "A VM shuts down or crashes. Stops you start from Homestack are skipped.", true},
	{"vm.started", "Virtual machines", "Started", "A VM starts.", false},
	{"system.started", "Server", "Homestack started", "Homestack starts, for example after the server reboots.", true},
	{"host", "Server", "Server alerts", "Alerts sent by the server's own scripts: ZFS, SMART, disk space, mover.", true},
}

// Config is what the notifier needs from the settings.
type Config struct {
	Webhook    string
	Enabled    map[string]bool // missing = the event's default
	ServerName string
	Ntfy       Ntfy // backup channel
}

// Active says whether anywhere to send to is set up.
func (c Config) Active() bool { return c.Webhook != "" || c.Ntfy.Enabled() }

// EventEnabled says whether an event kind is switched on.
func (c Config) EventEnabled(key string) bool {
	if v, ok := c.Enabled[key]; ok {
		return v
	}
	for _, e := range Events {
		if e.Key == key {
			return e.Default
		}
	}
	return false
}

// ---------- Webhook URLs ----------

var webhookHosts = map[string]bool{
	"discord.com": true, "discordapp.com": true,
	"canary.discord.com": true, "ptb.discord.com": true,
}

// ValidateWebhook checks that a URL is a Discord webhook, so Homestack never
// posts to anywhere else.
func ValidateWebhook(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !webhookHosts[strings.ToLower(u.Hostname())] || u.Port() != "" {
		return "", errors.New("That isn't a Discord webhook URL. It should start with https://discord.com/api/webhooks/.")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// api/webhooks/{id}/{token} or api/v10/webhooks/{id}/{token}
	if len(parts) >= 2 && parts[0] == "api" && strings.HasPrefix(parts[1], "v") {
		parts = append(parts[:1], parts[2:]...)
	}
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "webhooks" || parts[2] == "" || parts[3] == "" {
		return "", errors.New("That Discord URL is incomplete. Copy the whole webhook URL from Discord (Edit Channel → Integrations → Webhooks → Copy Webhook URL).")
	}
	if _, err := strconv.ParseUint(parts[2], 10, 64); err != nil {
		return "", errors.New("That Discord URL is incomplete. Copy the whole webhook URL from Discord.")
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// Hint describes a saved webhook without revealing its secret token.
func Hint(webhook string) string {
	u, err := url.Parse(webhook)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	id := ""
	for i, p := range parts {
		if p == "webhooks" && i+1 < len(parts) {
			id = parts[i+1]
		}
	}
	if len(id) > 6 {
		id = "…" + id[len(id)-6:]
	}
	return fmt.Sprintf("%s webhook %s", u.Hostname(), id)
}

// ---------- Sending ----------

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color"`
	Fields      []embedField `json:"fields,omitempty"`
	Footer      *struct {
		Text string `json:"text"`
	} `json:"footer,omitempty"`
	Timestamp string `json:"timestamp"`
}

type payload struct {
	Username        string   `json:"username"`
	Embeds          []embed  `json:"embeds"`
	AllowedMentions struct { // never ping anyone, whatever a container name says
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func buildPayload(m Message, server string) payload {
	lvl := m.Level
	if lvl == "" {
		lvl = Info
	}
	e := embed{
		Title:       clip(levelIcon[lvl]+" "+m.Title, 256),
		Description: clip(m.Body, 4000),
		Color:       levelColor[lvl],
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	for i, f := range m.Fields {
		if i == 25 {
			break
		}
		v := clip(f.Value, 1024)
		if v == "" {
			v = "–"
		}
		e.Fields = append(e.Fields, embedField{Name: clip(f.Name, 256), Value: v, Inline: f.Inline})
	}
	if server != "" {
		e.Footer = &struct {
			Text string `json:"text"`
		}{Text: clip(server, 2000)}
	}
	p := payload{Username: "Homestack", Embeds: []embed{e}}
	p.AllowedMentions.Parse = []string{}
	return p
}

// RateLimitError means Discord asked us to wait before sending again.
type RateLimitError struct{ After time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("Discord is rate limiting this webhook; try again in %s", e.After.Round(time.Second))
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Send posts one message straight away. It's used by the test button, the
// CLI and the background sender.
func Send(ctx context.Context, webhook string, m Message, server string) error {
	body, _ := json.Marshal(buildPayload(m, server))
	req, err := http.NewRequestWithContext(ctx, "POST", webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Homestack (https://github.com/homestack, notifications)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach Discord: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		var rl struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(data, &rl)
		after := time.Duration(rl.RetryAfter * float64(time.Second))
		if after <= 0 {
			if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil {
				after = time.Duration(s * float64(time.Second))
			}
		}
		if after <= 0 {
			after = 5 * time.Second
		}
		return &RateLimitError{After: after}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
		return errors.New("Discord doesn't recognise this webhook. It may have been deleted; create a new one and paste its URL")
	default:
		var de struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &de)
		if de.Message != "" {
			return fmt.Errorf("Discord refused the message (%d): %s", resp.StatusCode, de.Message)
		}
		return fmt.Errorf("Discord refused the message (HTTP %d)", resp.StatusCode)
	}
}

// ---------- Background notifier ----------

// Sent is one entry in the recent-notifications list on the settings page.
type Sent struct {
	Time  time.Time `json:"time"`
	Event string    `json:"event"`
	Level Level     `json:"level"`
	Title string    `json:"title"`
	OK    bool      `json:"ok"`
	Via   string    `json:"via,omitempty"` // discord, ntfy or discord+ntfy
	Error string    `json:"error,omitempty"`
}

// Notifier queues messages and sends them one at a time in the background,
// so a slow or unreachable Discord never holds anything else up.
type Notifier struct {
	config func() Config
	queue  chan Message

	mu     sync.Mutex
	last   map[string]time.Time // de-duplication key -> last sent
	recent []Sent

	send     func(ctx context.Context, webhook string, m Message, server string) error // swappable in tests
	sendNtfy func(ctx context.Context, n Ntfy, m Message, server, note string) error
}

// New creates a notifier. config is called for every message, so changes in
// the settings apply straight away.
func New(config func() Config) *Notifier {
	return &Notifier{config: config, queue: make(chan Message, 100), last: map[string]time.Time{}, send: Send, sendNtfy: SendNtfy}
}

// Notify queues a message if its event is switched on and it isn't a repeat.
// It never blocks.
func (n *Notifier) Notify(m Message) {
	if n == nil {
		return
	}
	cfg := n.config()
	if !cfg.Active() || (m.Event != "" && !cfg.EventEnabled(m.Event)) {
		return
	}
	if m.Key != "" && m.Cooldown > 0 {
		n.mu.Lock()
		if t, ok := n.last[m.Key]; ok && time.Since(t) < m.Cooldown {
			n.mu.Unlock()
			return
		}
		n.last[m.Key] = time.Now()
		// Forget old keys so the map can't grow forever.
		if len(n.last) > 2000 {
			for k, t := range n.last {
				if time.Since(t) > 24*time.Hour {
					delete(n.last, k)
				}
			}
		}
		n.mu.Unlock()
	}
	select {
	case n.queue <- m:
	default:
		log.Printf("notifications: queue full, dropped %q", m.Title)
	}
}

// Forget clears a de-duplication key, so the next message with it is sent.
func (n *Notifier) Forget(key string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	delete(n.last, key)
	n.mu.Unlock()
}

// Run sends queued messages until ctx ends. Discord allows about 30 messages
// a minute per webhook; a short gap between messages and honouring its
// retry_after keeps well inside that.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-n.queue:
			n.deliver(ctx, m)
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
		}
	}
}

func (n *Notifier) deliver(ctx context.Context, m Message) {
	cfg := n.config()
	o := route(ctx, cfg, m, func(ctx context.Context) error {
		var err error
		for attempt := range 4 {
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err = n.send(sctx, cfg.Webhook, m, cfg.ServerName)
			cancel()
			if err == nil {
				return nil
			}
			wait := time.Duration(attempt+1) * 5 * time.Second
			if rl, ok := errors.AsType[*RateLimitError](err); ok {
				wait = rl.After + 500*time.Millisecond
			} else if strings.Contains(err.Error(), "doesn't recognise") {
				return err // retrying won't help
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		return err
	}, func(ctx context.Context, note string) error {
		return n.sendNtfy(ctx, cfg.Ntfy, m, cfg.ServerName, note)
	})
	if o.via == "" {
		log.Printf("notifications: couldn't send %q: %v", m.Title, o.err())
	} else if o.ntfyErr != nil {
		log.Printf("notifications: ntfy copy of %q failed: %v", m.Title, o.ntfyErr)
	}
	s := Sent{Time: time.Now(), Event: m.Event, Level: m.Level, Title: m.Title, OK: o.via != "", Via: o.via}
	switch {
	case o.via == "":
		s.Error = errString(o.err())
	case o.discordErr != nil:
		s.Error = "Discord: " + o.discordErr.Error() // shown even when ntfy got it, so a broken webhook is noticed
	case o.ntfyErr != nil:
		s.Error = "ntfy: " + o.ntfyErr.Error()
	}
	n.record(s)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (n *Notifier) record(s Sent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.recent = append([]Sent{s}, n.recent...)
	if len(n.recent) > 20 {
		n.recent = n.recent[:20]
	}
}

// Recent returns the last messages sent (newest first).
func (n *Notifier) Recent() []Sent {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := append([]Sent(nil), n.recent...)
	if out == nil {
		out = []Sent{}
	}
	return out
}

// Record adds a message sent outside the queue (the test button) to the list.
func (n *Notifier) Record(m Message, err error) {
	n.record(Sent{Time: time.Now(), Event: m.Event, Level: m.Level, Title: m.Title, OK: err == nil, Error: errString(err)})
}

// SetNtfySenderForTest replaces how ntfy messages are delivered (tests only).
func SetNtfySenderForTest(n *Notifier, fn func(ctx context.Context, c Ntfy, m Message, server, note string) error) {
	n.sendNtfy = fn
}

// RecordVia adds a message sent outside the queue, with where it went.
func (n *Notifier) RecordVia(m Message, via string, err error) {
	n.record(Sent{Time: time.Now(), Event: m.Event, Level: m.Level, Title: m.Title, OK: err == nil, Via: via, Error: errString(err)})
}

// SetSenderForTest replaces how messages are delivered (tests only).
func SetSenderForTest(n *Notifier, fn func(ctx context.Context, webhook string, m Message, server string) error) {
	n.send = fn
}
