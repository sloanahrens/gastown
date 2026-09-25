package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/events"
)

// Start used to run the first prune inline, and the prune waits up to 30s for
// the events writer lock. A wedged writer must not stall daemon startup, and
// Stop must not wait out the lock timeout either (claude-9jq review).
func TestKRCPruner_StartAndStopDoNotBlockOnHeldLock(t *testing.T) {
	townRoot := t.TempDir()
	eventsPath := filepath.Join(townRoot, events.EventsFile)
	expired := `{"ts":"2000-01-01T00:00:00Z","type":"sling","actor":"old"}` + "\n"
	if err := os.WriteFile(eventsPath, []byte(expired), 0o644); err != nil {
		t.Fatal(err)
	}

	writer := flock.New(eventsPath + ".lock")
	if err := writer.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Unlock() }()

	p, err := NewKRCPruner(townRoot, t.Logf)
	if err != nil {
		t.Fatalf("NewKRCPruner: %v", err)
	}

	start := time.Now()
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Start blocked %v on a held events lock", d)
	}

	time.Sleep(200 * time.Millisecond) // let the first prune start waiting
	stopStart := time.Now()
	p.Stop()
	if d := time.Since(stopStart); d > 2*time.Second {
		t.Fatalf("Stop blocked %v behind a prune waiting on the lock", d)
	}
}

// The first prune still runs promptly after Start when the lock is free.
func TestKRCPruner_StartPrunesInBackground(t *testing.T) {
	townRoot := t.TempDir()
	eventsPath := filepath.Join(townRoot, events.EventsFile)
	expired := `{"ts":"2000-01-01T00:00:00Z","type":"sling","actor":"old"}` + "\n"
	if err := os.WriteFile(eventsPath, []byte(expired), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := NewKRCPruner(townRoot, t.Logf)
	if err != nil {
		t.Fatalf("NewKRCPruner: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(eventsPath); err == nil && len(data) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("first prune did not remove the expired event")
}
