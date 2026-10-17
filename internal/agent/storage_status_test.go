package agent

import (
	"testing"
	"time"
)

// `snapraid status` is read again after 30 minutes, or as soon as a sync or
// check has finished (or the content file was saved) since the last read.
func TestStatusStale(t *testing.T) {
	read := time.Date(2026, 10, 17, 13, 37, 0, 0, time.UTC)
	synced := read.Add(2 * time.Minute)
	if statusStale(read, read.Add(5*time.Minute), read.Add(-time.Hour), time.Time{}) {
		t.Error("nothing new: keep it")
	}
	if !statusStale(read, read.Add(5*time.Minute), synced) {
		t.Error("a sync finished after it was read")
	}
	if !statusStale(read, read.Add(31*time.Minute)) {
		t.Error("30 minutes old")
	}
	if !statusStale(time.Time{}, read) {
		t.Error("never read")
	}
}
