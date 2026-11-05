package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/reaper"
)

// eventsRetentionStep is how the pass's cycle recorded the events retention
// step.
func eventsRetentionStep(t *testing.T, cycle *dogCycle) dogStep {
	t.Helper()
	for _, s := range cycle.steps {
		if s.name == "events-retention" {
			return s
		}
	}
	t.Fatal("the pass recorded no events-retention step")
	return dogStep{}
}

// TestWispReaperEventsRetentionStepIsOffWhenUnset: an absent events_retention
// leaves the step off, and the cycle says so rather than reporting a pass.
func TestWispReaperEventsRetentionStepIsOffWhenUnset(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)

	p.d.reapWispsInline(&WispReaperConfig{Databases: []string{"beads_db"}}, 0, 0, 0, p.cycle)

	if s := eventsRetentionStep(t, p.cycle); s.result != dogStepSkipped {
		t.Errorf("events-retention step = %+v, want skipped", s)
	}
	if !strings.Contains(p.log.String(), "events retention skipped") {
		t.Errorf("log = %q, want the step reported as skipped", p.log.String())
	}
}

// TestWispReaperEventsRetentionStepFailsOnAPerDatabaseError: a database whose
// retention pass cannot run fails the step and says how many databases failed,
// so a broken purge reaches the cycle's receipt instead of closing as a pass.
func TestWispReaperEventsRetentionStepFailsOnAPerDatabaseError(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)
	p.d.reaperOpenDBFn = func(string, time.Duration, time.Duration) (*beadsql.DB, error) {
		return nil, errors.New("dolt unavailable")
	}

	p.d.reapWispsInline(&WispReaperConfig{
		Databases:          []string{"beads_db"},
		EventsRetentionStr: "30d",
	}, 0, 0, 0, p.cycle)

	if s := eventsRetentionStep(t, p.cycle); s.result != dogStepFailed {
		t.Errorf("events-retention step = %+v, want failed", s)
	} else if s.reason != "1 databases had events retention errors" {
		t.Errorf("events-retention reason = %q, want the failing database counted", s.reason)
	}
	if !strings.Contains(p.log.String(), "beads_db: events retention connect error") {
		t.Errorf("log = %q, want the database and its retention failure named", p.log.String())
	}
}

// TestWispReaperEventsRetentionStepRefusesABadValue: a value the config cannot
// parse fails the step and runs no pass.
func TestWispReaperEventsRetentionStepRefusesABadValue(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)

	p.d.reapWispsInline(&WispReaperConfig{
		Databases:          []string{"beads_db"},
		EventsRetentionStr: "soon",
	}, 0, 0, 0, p.cycle)

	if s := eventsRetentionStep(t, p.cycle); s.result != dogStepFailed {
		t.Errorf("events-retention step = %+v, want failed", s)
	}
	if !strings.Contains(p.log.String(), "patrols.wisp_reaper.events_retention") {
		t.Errorf("log = %q, want the refusal to name the field", p.log.String())
	}
}

// writeEventsRetentionState writes the pass's maintenance state file under its
// town root, so the events retention step reads body.
func writeEventsRetentionState(t *testing.T, townRoot, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create daemon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "maintenance_state.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write maintenance state: %v", err)
	}
}

// TestWispReaperEventsRetentionStepFailsOnACorruptStateFile: a state file that
// exists but cannot be parsed is refused, so the database is counted as failed
// and the step fails rather than passing as skipped (gt-2i850).
func TestWispReaperEventsRetentionStepFailsOnACorruptStateFile(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)
	writeEventsRetentionState(t, p.d.config.TownRoot, "{not json")
	p.d.reaperOpenDBFn = func(string, time.Duration, time.Duration) (*beadsql.DB, error) {
		return openEmptySweepDB(t), nil
	}

	p.d.reapWispsInline(&WispReaperConfig{
		Databases:          []string{"beads_db"},
		EventsRetentionStr: "30d",
	}, 0, 0, 0, p.cycle)

	if s := eventsRetentionStep(t, p.cycle); s.result != dogStepFailed {
		t.Errorf("events-retention step = %+v, want failed on a corrupt state file", s)
	} else if s.reason != "1 databases had events retention errors" {
		t.Errorf("events-retention reason = %q, want the refusing database counted as failed", s.reason)
	}
	if !strings.Contains(p.log.String(), "beads_db: events retention error") {
		t.Errorf("log = %q, want the database and its refusal named", p.log.String())
	}
}

// TestWispReaperEventsRetentionStepFailsOnACorruptWatermark: a watermark with
// a created_at and no ids fails Validate, so the database is counted as failed
// and the step fails rather than passing as skipped (gt-2i850).
func TestWispReaperEventsRetentionStepFailsOnACorruptWatermark(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)
	writeEventsRetentionState(t, p.d.config.TownRoot,
		`{"archive_events_watermarks":{"beads_db":{"created_at":"2029-12-14 11:00:00"}}}`)
	p.d.reaperOpenDBFn = func(string, time.Duration, time.Duration) (*beadsql.DB, error) {
		return openEmptySweepDB(t), nil
	}

	p.d.reapWispsInline(&WispReaperConfig{
		Databases:          []string{"beads_db"},
		EventsRetentionStr: "30d",
	}, 0, 0, 0, p.cycle)

	if s := eventsRetentionStep(t, p.cycle); s.result != dogStepFailed {
		t.Errorf("events-retention step = %+v, want failed on a corrupt watermark", s)
	} else if s.reason != "1 databases had events retention errors" {
		t.Errorf("events-retention reason = %q, want the refusing database counted as failed", s.reason)
	}
	if !strings.Contains(p.log.String(), "lists no ids") {
		t.Errorf("log = %q, want the watermark's fault named", p.log.String())
	}
}

// TestEventsRetentionWindowStartMatchesTheArchiveSince pins reaper's copy of
// the events archive lookback to the archive's own Since: retention must bound
// its delete exactly where the archive's next read starts, so changing the
// window in either place turns this test red (gt-2i850).
func TestEventsRetentionWindowStartMatchesTheArchiveSince(t *testing.T) {
	t.Parallel()
	for _, createdAt := range []string{
		"2030-01-15 02:00:00",
		"2030-01-15 02:00:00.5",
		"2030-01-15T02:00:00Z",
		"2030-01-15T02:00:00.25+00:00",
	} {
		t.Run(createdAt, func(t *testing.T) {
			t.Parallel()
			since, err := (EventsArchiveWatermark{CreatedAt: createdAt}).Since()
			if err != nil {
				t.Fatalf("Since(%q): %v", createdAt, err)
			}
			want, err := parseEventsTimestamp(since)
			if err != nil {
				t.Fatalf("parse Since(%q) = %q: %v", createdAt, since, err)
			}
			got, err := reaper.EventsArchiveWindowStart(createdAt)
			if err != nil {
				t.Fatalf("reaper.EventsArchiveWindowStart(%q): %v", createdAt, err)
			}
			if !got.Equal(want) {
				t.Errorf("retention window start = %s, archive Since = %s for %q", got, want, createdAt)
			}
		})
	}
}

// TestWispReaperEventsRetentionStepSkipsWhileWaitingForTheArchive: a database
// whose events archive has no watermark deletes nothing, is logged as waiting
// for the archive, and makes the step skipped rather than passed (gt-2i850).
func TestWispReaperEventsRetentionStepSkipsWhileWaitingForTheArchive(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)
	p.d.reaperOpenDBFn = func(string, time.Duration, time.Duration) (*beadsql.DB, error) {
		return openEmptySweepDB(t), nil
	}

	p.d.reapWispsInline(&WispReaperConfig{
		Databases:          []string{"beads_db"},
		EventsRetentionStr: "30d",
	}, 0, 0, 0, p.cycle)

	if s := eventsRetentionStep(t, p.cycle); s.result != dogStepSkipped {
		t.Errorf("events-retention step = %+v, want skipped while the archive has no watermark", s)
	} else if s.reason != "1 databases waiting for the events archive watermark" {
		t.Errorf("events-retention reason = %q, want the waiting database counted", s.reason)
	}
	if !strings.Contains(p.log.String(), "beads_db: events retention: skipped: waiting for the events archive watermark") {
		t.Errorf("log = %q, want the database reported as waiting for the archive", p.log.String())
	}
}
