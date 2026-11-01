package daemon

import (
	"strings"
	"testing"
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
