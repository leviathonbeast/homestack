package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ntfy (https://ntfy.sh) is the backup channel: push notifications to the
// ntfy app on a phone, used when Discord can't be reached, or alongside it.

// Ntfy modes: when a message goes to ntfy as well as Discord.
const (
	NtfyFallback  = "fallback"  // only when Discord fails (or isn't set up)
	NtfyImportant = "important" // warnings and errors always, the rest on failure
	NtfyAll       = "all"       // every message
)

// Ntfy is the ntfy part of the settings.
type Ntfy struct {
	Server string // https://ntfy.sh, or your own
	Topic  string
	Token  string // optional access token (tk_...)
	Mode   string
}

// Enabled says whether ntfy is set up.
func (n Ntfy) Enabled() bool { return n.Topic != "" }

const DefaultNtfyServer = "https://ntfy.sh"

var reTopic = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidateNtfy checks the server address and topic name.
func ValidateNtfy(server, topic string) (string, string, error) {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server == "" {
		server = DefaultNtfyServer
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", errors.New("The ntfy server should look like https://ntfy.sh.")
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Plain http only for a server on your own network.
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !(ip.IsPrivate() || ip.IsLoopback()) {
			return "", "", errors.New("Use https for the ntfy server (http is only allowed for a private IP address on your network).")
		}
	default:
		return "", "", errors.New("The ntfy server should start with https://.")
	}
	topic = strings.TrimSpace(topic)
	if !reTopic.MatchString(topic) {
		return "", "", errors.New("The topic can only use letters, numbers, - and _ (up to 64).")
	}
	return server, topic, nil
}

// NtfyMode normalises a mode name.
func NtfyMode(s string) string {
	switch s {
	case NtfyImportant, NtfyAll:
		return s
	}
	return NtfyFallback
}

var ntfyPriority = map[Level]int{Info: 2, Success: 3, Warning: 4, Error: 5}
var ntfyTag = map[Level]string{Info: "information_source", Success: "white_check_mark", Warning: "warning", Error: "rotating_light"}

// plain turns Discord markdown into plain text for a phone notification.
func plain(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	return strings.ReplaceAll(s, "`", "")
}

// SendNtfy posts one message to ntfy. note, if set, is added at the end (why
// it came here instead of Discord).
func SendNtfy(ctx context.Context, n Ntfy, m Message, server, note string) error {
	var b strings.Builder
	b.WriteString(plain(m.Body))
	for _, f := range m.Fields {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s: %s", plain(f.Name), plain(f.Value))
	}
	if note != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(note)
	}
	msg := b.String()
	if msg == "" {
		msg = plain(m.Title)
	}
	title := plain(m.Title)
	if server != "" {
		title = "[" + server + "] " + title
	}
	lvl := m.Level
	if lvl == "" {
		lvl = Info
	}
	tags := []string{ntfyTag[lvl]}
	if note != "" {
		tags = append(tags, "discord-down")
	}
	body, _ := json.Marshal(map[string]any{
		"topic": n.Topic, "title": clip(title, 250), "message": clip(msg, 3900),
		"priority": ntfyPriority[lvl], "tags": tags,
	})
	srv := n.Server
	if srv == "" {
		srv = DefaultNtfyServer
	}
	req, err := http.NewRequestWithContext(ctx, "POST", srv+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Homestack (notifications)")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach ntfy: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return errors.New("ntfy is rate limiting this server (the free ntfy.sh allows a few hundred messages a day)")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errors.New("ntfy refused the message: the topic is reserved or the access token is wrong")
	default:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error != "" {
			return fmt.Errorf("ntfy refused the message (%d): %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("ntfy refused the message (HTTP %d)", resp.StatusCode)
	}
}

// ntfyUpfront says whether a message goes to ntfy regardless of Discord (sent
// straight away, alongside it), rather than only as a fallback.
func ntfyUpfront(c Config, m Message) bool {
	if !c.Ntfy.Enabled() {
		return false
	}
	if c.Webhook == "" {
		return true
	}
	switch NtfyMode(c.Ntfy.Mode) {
	case NtfyAll:
		return true
	case NtfyImportant:
		return m.Level == Warning || m.Level == Error
	}
	return false
}

func fallbackNote(discordErr error) string {
	return "Sent here because Discord failed: " + discordErr.Error()
}

// outcome is what happened to one message.
type outcome struct {
	via        string // discord, ntfy, discord+ntfy or ""
	discordErr error
	ntfyErr    error
	tried      bool // Discord was tried
}

// route sends a message by the settings: ntfy at the same time as Discord
// when it's wanted anyway, otherwise only if Discord fails.
func route(ctx context.Context, c Config, m Message,
	discord func(context.Context) error, ntfy func(ctx context.Context, note string) error) outcome {
	o := outcome{tried: c.Webhook != ""}
	up := ntfyUpfront(c, m)
	ntfyDone := make(chan error, 1)
	if up {
		go func() {
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			ntfyDone <- ntfy(sctx, "")
		}()
	}
	if o.tried {
		o.discordErr = discord(ctx)
	}
	ntfyTried := up
	if up {
		o.ntfyErr = <-ntfyDone
	} else if c.Ntfy.Enabled() && o.tried && o.discordErr != nil {
		ntfyTried = true
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		o.ntfyErr = ntfy(sctx, fallbackNote(o.discordErr))
		cancel()
	}
	if o.tried && o.discordErr == nil {
		o.via = "discord"
	}
	if ntfyTried && o.ntfyErr == nil {
		if o.via != "" {
			o.via += "+ntfy"
		} else {
			o.via = "ntfy"
		}
	}
	return o
}

// err is the error to report when nothing got through.
func (o outcome) err() error {
	switch {
	case o.via != "":
		return nil
	case o.discordErr != nil && o.ntfyErr != nil:
		return fmt.Errorf("%v; ntfy failed too: %v", o.discordErr, o.ntfyErr)
	case o.discordErr != nil:
		return o.discordErr
	default:
		return o.ntfyErr
	}
}

// DeliverNow sends one message to Discord and/or ntfy as the settings say,
// waiting for the answers. Used by the CLI and the host agent. It succeeds if
// the message reached at least one place.
func DeliverNow(ctx context.Context, c Config, m Message) (via string, err error) {
	o := route(ctx, c, m, func(ctx context.Context) error {
		var derr error
		for attempt := 0; ; attempt++ {
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			derr = Send(sctx, c.Webhook, m, c.ServerName)
			cancel()
			var rl *RateLimitError
			if derr == nil || attempt == 2 || !errors.As(derr, &rl) {
				return derr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(rl.After + 500*time.Millisecond):
			}
		}
	}, func(ctx context.Context, note string) error {
		return SendNtfy(ctx, c.Ntfy, m, c.ServerName, note)
	})
	return o.via, o.err()
}
