package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/events"
)

func TestEventsPruneSettings(t *testing.T) {
	t.Parallel()
	interval, opts := eventsPruneSettings(nil)
	if interval != time.Hour || opts.MaxAge != 7*24*time.Hour || opts.MaxBytes != 16<<20 {
		t.Errorf("defaults = %v %+v", interval, opts)
	}

	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{EventsPrune: &EventsPruneConfig{
		Enabled: true, IntervalStr: "10m", MaxAgeStr: "48h", MaxBytes: 1024,
	}}}
	interval, opts = eventsPruneSettings(cfg)
	if interval != 10*time.Minute || opts.MaxAge != 48*time.Hour || opts.MaxBytes != 1024 {
		t.Errorf("configured = %v %+v", interval, opts)
	}

	cfg.Patrols.EventsPrune = &EventsPruneConfig{Enabled: true, IntervalStr: "soon", MaxAgeStr: "-1h"}
	interval, opts = eventsPruneSettings(cfg)
	if interval != time.Hour || opts.MaxAge != 7*24*time.Hour {
		t.Errorf("invalid values should fall back to defaults, got %v %+v", interval, opts)
	}
}

func TestEventsPruneDefaultsOn(t *testing.T) {
	t.Parallel()
	if !IsPatrolEnabled(nil, "events_prune") || !IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "events_prune") {
		t.Error("events_prune must run when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{EventsPrune: &EventsPruneConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "events_prune") {
		t.Error("an explicit enabled=false must turn events_prune off")
	}
}

// The heartbeat prunes once per interval, measured from the persisted
// last-run time, and leaves in-window events alone.
func TestPruneEventsLog_RunsWhenDue(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, events.EventsFile)
	clock := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	line := func(age time.Duration) string {
		return fmt.Sprintf(`{"ts":%q,"type":"mail"}`+"\n", clock.Now().Add(-age).Format(time.RFC3339))
	}
	write := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{EventsPrune: &EventsPruneConfig{
		Enabled: true, IntervalStr: "1h", MaxAgeStr: "2h",
	}}}
	d := &Daemon{config: &Config{TownRoot: town}, patrolConfig: cfg, logger: log.New(io.Discard, "", 0), clock: clock}

	b := line(90 * time.Minute)
	write(line(3*time.Hour) + b)
	d.pruneEventsLog()
	if got := read(); got != b {
		t.Fatalf("after first prune = %q, want %q", got, b)
	}

	// b is now past max_age, but the interval has not elapsed.
	clock.Advance(45 * time.Minute)
	d.pruneEventsLog()
	if got := read(); got != b {
		t.Fatalf("pruned before the interval elapsed: %q", got)
	}

	clock.Advance(20 * time.Minute)
	c := line(time.Minute)
	write(c)
	d.pruneEventsLog()
	if got := read(); got != c {
		t.Errorf("once due = %q, want %q", got, c)
	}
}
