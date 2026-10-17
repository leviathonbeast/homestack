package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"homestack/internal/docker"
	"homestack/internal/notify"
)

type capture struct {
	mu  sync.Mutex
	got []notify.Message
}

func newCapturingNotifier(t *testing.T) (*notify.Notifier, *capture, context.CancelFunc) {
	t.Helper()
	c := &capture{}
	n := notify.New(func() notify.Config {
		return notify.Config{Webhook: "https://discord.com/api/webhooks/1/t"}
	})
	notify.SetSenderForTest(n, func(ctx context.Context, webhook string, m notify.Message, server string) error {
		c.mu.Lock()
		c.got = append(c.got, m)
		c.mu.Unlock()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go n.Run(ctx)
	return n, c, cancel
}

func (c *capture) titles(wait int) []string {
	deadline := time.Now().Add(time.Duration(wait)*1100*time.Millisecond + time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		l := len(c.got)
		c.mu.Unlock()
		if l >= wait {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.got {
		out = append(out, m.Title)
	}
	return out
}

func ev(action, id, name, exit string) docker.Event {
	var e docker.Event
	e.Type = "container"
	e.Action = action
	e.Actor.ID = id
	e.Actor.Attributes = map[string]string{"name": name, "image": "img:1", docker.LabelProject: "media"}
	if exit != "" {
		e.Actor.Attributes["exitCode"] = exit
	}
	return e
}

func TestContainerWatchTellsCrashesFromStops(t *testing.T) {
	n, c, cancel := newCapturingNotifier(t)
	defer cancel()
	h := newContainerWatch().handle(n)

	// Deliberate stop: kill then die. No alert.
	h(ev("kill", "aaa", "jellyfin", ""))
	h(ev("die", "aaa", "jellyfin", "143"))
	// Clean exit of a one-shot job. No alert.
	h(ev("die", "bbb", "backup-job", "0"))
	// A real crash.
	h(ev("die", "ccc", "sonarr", "1"))

	got := c.titles(1)
	if strings.Join(got, "|") != "Crashed: sonarr" {
		t.Fatalf("got %v", got)
	}
}

func TestContainerWatchRestartLoop(t *testing.T) {
	n, c, cancel := newCapturingNotifier(t)
	defer cancel()
	h := newContainerWatch().handle(n)
	for range 5 {
		h(ev("die", "ddd", "radarr", "1"))
		h(ev("start", "ddd", "radarr", ""))
	}
	got := c.titles(2)
	// First crash is reported, repeats are grouped, then one "keeps restarting".
	if strings.Join(got, "|") != "Crashed: radarr|Keeps restarting: radarr" {
		t.Fatalf("got %v", got)
	}
}

func TestContainerWatchHealth(t *testing.T) {
	n, c, cancel := newCapturingNotifier(t)
	defer cancel()
	h := newContainerWatch().handle(n)
	h(ev("health_status: healthy", "eee", "gitea", ""))   // healthy from the start: nothing
	h(ev("health_status: unhealthy", "eee", "gitea", "")) // report
	h(ev("health_status: unhealthy", "eee", "gitea", "")) // still unhealthy: nothing
	h(ev("health_status: healthy", "eee", "gitea", ""))   // recovered
	h(ev("oom", "fff", "immich", ""))
	got := c.titles(3)
	want := "Unhealthy: gitea|Healthy again: gitea|Out of memory: immich"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %v", got)
	}
}

func TestUpdateAndVMNotifications(t *testing.T) {
	n, c, cancel := newCapturingNotifier(t)
	defer cancel()
	s := &Server{notifier: n, updNotified: map[string]bool{}, vmIntent: map[string]time.Time{}}

	s.notifyNewUpdates(map[string]updateResult{
		"a:latest": {Image: "a:latest", Available: true},
		"b:latest": {Image: "b:latest", Available: true},
		"c:latest": {Image: "c:latest"},
	})
	// Same results again: nothing new to report.
	s.notifyNewUpdates(map[string]updateResult{
		"a:latest": {Image: "a:latest", Available: true},
		"b:latest": {Image: "b:latest", Available: true},
	})
	// a was updated (no longer available), then gets a new update: report it again.
	s.notifyNewUpdates(map[string]updateResult{"b:latest": {Image: "b:latest", Available: true}})
	s.notifyNewUpdates(map[string]updateResult{
		"a:latest": {Image: "a:latest", Available: true},
		"b:latest": {Image: "b:latest", Available: true},
	})

	s.vmChanged("StepCA", "running", "shutoff") // from inside the VM: report
	s.markVMIntent("OpenSUSE")
	s.vmChanged("OpenSUSE", "running", "shutoff") // from Homestack: skip
	s.vmChanged("gui", "running", "crashed")      // always report

	got := c.titles(4)
	want := "2 image updates available|1 image update available|VM stopped: StepCA|VM crashed: gui"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %v", got)
	}
}
