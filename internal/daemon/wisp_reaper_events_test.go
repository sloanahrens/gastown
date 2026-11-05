package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
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
