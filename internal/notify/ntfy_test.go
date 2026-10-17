package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeNtfy struct {
	mu   sync.Mutex
	got  []map[string]any
	auth []string
}

func (f *fakeNtfy) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.got = append(f.got, m)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func discordStub(t *testing.T, status int) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	t.Cleanup(s.Close)
	return s.URL
}

func TestDeliverNowFallsBackToNtfy(t *testing.T) {
	f := &fakeNtfy{}
	ns := f.server(t)
	cfg := Config{Webhook: discordStub(t, 404), ServerName: "lucy",
		Ntfy: Ntfy{Server: ns.URL, Topic: "lucy-abc", Token: "tk_secret", Mode: NtfyFallback}}
	via, err := DeliverNow(context.Background(), cfg, Message{Level: Error, Title: "Disk **hot**", Body: "disk2 is **52°C**"})
	if err != nil || via != "ntfy" {
		t.Fatalf("via=%q err=%v", via, err)
	}
	m := f.got[0]
	if m["topic"] != "lucy-abc" || m["title"] != "[lucy] Disk hot" || m["priority"].(float64) != 5 {
		t.Fatalf("payload %v", m)
	}
	if !strings.Contains(m["message"].(string), "disk2 is 52°C") || !strings.Contains(m["message"].(string), "Discord failed") {
		t.Fatalf("message %q", m["message"])
	}
	if f.auth[0] != "Bearer tk_secret" {
		t.Fatal("token not sent")
	}
}

func TestDeliverNowModes(t *testing.T) {
	f := &fakeNtfy{}
	ns := f.server(t)
	ok := discordStub(t, 204)
	base := Config{Webhook: ok, Ntfy: Ntfy{Server: ns.URL, Topic: "t"}}

	if via, _ := DeliverNow(context.Background(), base, Message{Level: Error, Title: "x"}); via != "discord" || len(f.got) != 0 {
		t.Fatalf("fallback mode sent to ntfy while Discord worked: %q", via)
	}
	base.Ntfy.Mode = NtfyImportant
	DeliverNow(context.Background(), base, Message{Level: Info, Title: "info"})
	if via, _ := DeliverNow(context.Background(), base, Message{Level: Warning, Title: "warn"}); via != "discord+ntfy" || len(f.got) != 1 {
		t.Fatalf("important mode: via=%q sent=%d", via, len(f.got))
	}
	// No Discord at all: ntfy is the only channel.
	if via, err := DeliverNow(context.Background(), Config{Ntfy: Ntfy{Server: ns.URL, Topic: "t"}}, Message{Title: "y"}); via != "ntfy" || err != nil {
		t.Fatalf("ntfy only: %q %v", via, err)
	}
	// Both down: an error naming both.
	_, err := DeliverNow(context.Background(), Config{Webhook: discordStub(t, 500), Ntfy: Ntfy{Server: discordStub(t, 500), Topic: "t"}}, Message{Title: "z"})
	if err == nil || !strings.Contains(err.Error(), "ntfy failed too") {
		t.Fatalf("err %v", err)
	}
}

func TestNotifierFallback(t *testing.T) {
	n := New(func() Config { return Config{Webhook: "https://discord.com/x", Ntfy: Ntfy{Topic: "t"}} })
	SetSenderForTest(n, func(ctx context.Context, w string, m Message, s string) error {
		return errors.New("Discord doesn't recognise this webhook")
	})
	var note string
	SetNtfySenderForTest(n, func(ctx context.Context, c Ntfy, m Message, s, nt string) error { note = nt; return nil })
	n.deliver(context.Background(), Message{Title: "t", Level: Error})
	r := n.Recent()[0]
	if !r.OK || r.Via != "ntfy" || r.Error == "" || !strings.Contains(note, "Discord failed") {
		t.Fatalf("%+v note=%q", r, note)
	}
	_ = time.Second
}

func TestValidateNtfy(t *testing.T) {
	if s, tp, err := ValidateNtfy("", "lucy-Alerts_1"); err != nil || s != DefaultNtfyServer || tp != "lucy-Alerts_1" {
		t.Fatal(s, tp, err)
	}
	for _, bad := range [][2]string{{"http://ntfy.sh", "a"}, {"ftp://x", "a"}, {"https://ntfy.sh", "has space"}, {"https://ntfy.sh", ""}} {
		if _, _, err := ValidateNtfy(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, _, err := ValidateNtfy("http://10.0.10.5:8090", "a"); err != nil {
		t.Errorf("LAN http refused: %v", err)
	}
}

func TestAllModeSendsNtfyWithoutWaitingForDiscord(t *testing.T) {
	n := New(func() Config { return Config{Webhook: "https://discord.com/x", Ntfy: Ntfy{Topic: "t", Mode: NtfyAll}} })
	start := time.Now()
	var ntfyAt time.Duration
	SetSenderForTest(n, func(ctx context.Context, w string, m Message, s string) error {
		time.Sleep(300 * time.Millisecond) // a slow Discord
		return nil
	})
	SetNtfySenderForTest(n, func(ctx context.Context, c Ntfy, m Message, s, nt string) error {
		ntfyAt = time.Since(start)
		return nil
	})
	n.deliver(context.Background(), Message{Title: "t", Level: Info})
	if r := n.Recent()[0]; r.Via != "discord+ntfy" || r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if ntfyAt > 150*time.Millisecond {
		t.Fatalf("ntfy waited for Discord (%v)", ntfyAt)
	}
}
