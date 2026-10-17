package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateWebhook(t *testing.T) {
	good := []string{
		"https://discord.com/api/webhooks/123456789012345678/abcDEF-token_x",
		"https://discordapp.com/api/webhooks/123/tok?wait=true",
		"https://canary.discord.com/api/v10/webhooks/123/tok",
		"  https://discord.com/api/webhooks/123/tok  ",
	}
	for _, u := range good {
		out, err := ValidateWebhook(u)
		if err != nil {
			t.Errorf("%q: unexpected error %v", u, err)
		}
		if strings.Contains(out, "?") {
			t.Errorf("%q: query not stripped: %s", u, out)
		}
	}
	bad := []string{
		"http://discord.com/api/webhooks/123/tok",       // not https
		"https://evil.com/api/webhooks/123/tok",         // wrong host
		"https://discord.com.evil.com/api/webhooks/1/t", // lookalike host
		"https://discord.com:8443/api/webhooks/123/tok", // port
		"https://discord.com/api/webhooks/123",          // no token
		"https://discord.com/api/webhooks/abc/tok",      // id not numeric
		"https://discord.com/api/channels/123/messages", // not a webhook
		"https://user:pw@evil.com/api/webhooks/123/tok", // userinfo trick
		"",
	}
	for _, u := range bad {
		if _, err := ValidateWebhook(u); err == nil {
			t.Errorf("%q: expected an error", u)
		}
	}
}

func TestHintHidesToken(t *testing.T) {
	h := Hint("https://discord.com/api/webhooks/123456789012345678/SECRETTOKEN")
	if strings.Contains(h, "SECRET") || !strings.Contains(h, "345678") {
		t.Fatalf("bad hint %q", h)
	}
}

func TestPayloadNeverPings(t *testing.T) {
	p := buildPayload(Message{Title: "@everyone look", Body: strings.Repeat("x", 5000), Level: Error}, "lucy")
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"allowed_mentions":{"parse":[]}`) {
		t.Fatalf("allowed_mentions missing: %s", b)
	}
	if len([]rune(p.Embeds[0].Description)) > 4000 {
		t.Fatal("description not clipped")
	}
	if p.Embeds[0].Color != levelColor[Error] || p.Embeds[0].Footer.Text != "lucy" {
		t.Fatal("colour or footer wrong")
	}
}

func TestSendAndRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "Hello") {
			t.Errorf("body missing title: %s", body)
		}
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			w.Write([]byte(`{"message":"You are being rate limited.","retry_after":0.2,"global":false}`))
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	err := Send(context.Background(), srv.URL, Message{Title: "Hello"}, "")
	rl, ok := err.(*RateLimitError)
	if !ok || rl.After != 200*time.Millisecond {
		t.Fatalf("expected rate limit error, got %v", err)
	}
	if err := Send(context.Background(), srv.URL, Message{Title: "Hello"}, ""); err != nil {
		t.Fatalf("second send: %v", err)
	}
}

func TestSendDeletedWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Unknown Webhook","code":10015}`))
	}))
	defer srv.Close()
	err := Send(context.Background(), srv.URL, Message{Title: "x"}, "")
	if err == nil || !strings.Contains(err.Error(), "doesn't recognise") {
		t.Fatalf("got %v", err)
	}
}

func TestNotifierFiltersAndDedups(t *testing.T) {
	var mu sync.Mutex
	var got []string
	cfg := Config{Webhook: "https://discord.com/api/webhooks/1/t", Enabled: map[string]bool{"stack.updated": false, "vm.started": true}}
	n := New(func() Config { return cfg })
	n.send = func(ctx context.Context, webhook string, m Message, server string) error {
		mu.Lock()
		got = append(got, m.Title)
		mu.Unlock()
		return nil
	}
	ctx := t.Context()
	go n.Run(ctx)

	n.Notify(Message{Event: "stack.updated", Title: "off by setting"})
	n.Notify(Message{Event: "vm.started", Title: "on by setting"}) // default off, switched on
	n.Notify(Message{Event: "container.crash", Title: "crash 1", Key: "c", Cooldown: time.Hour})
	n.Notify(Message{Event: "container.crash", Title: "crash 2", Key: "c", Cooldown: time.Hour}) // repeat
	n.Notify(Message{Event: "unknown.kind", Title: "unknown"})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		l := len(got)
		mu.Unlock()
		if l >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "|") != "on by setting|crash 1" {
		t.Fatalf("sent %v", got)
	}
	if r := n.Recent(); len(r) != 2 || !r[0].OK {
		t.Fatalf("recent = %+v", r)
	}
}

func TestNotifierOffWithoutWebhook(t *testing.T) {
	n := New(func() Config { return Config{} })
	n.Notify(Message{Event: "container.crash", Title: "x"})
	if len(n.queue) != 0 {
		t.Fatal("queued a message with no webhook")
	}
}
