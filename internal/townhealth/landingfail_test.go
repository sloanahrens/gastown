package townhealth

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A landing that keeps failing is a rig's landing field degraded, with the
// facts that say which landing and why: the bead, the stage, the run of
// failures, the next retry and the error. One failure is an incident the
// worker retries on its own (gt-fn9e6.44).
func TestLandingFieldDegradedWhileALandingFails(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.landings = []RigLandings{
		// One failure in a row: still green, the worker is retrying.
		{Rig: "once", Failing: []FailingLanding{{Bead: "gt-once", Stage: "push", Failures: 1, NextTry: ago(-time.Minute), Error: "refused"}}},
		// Two in a row: the landing is not clearing.
		{Rig: "twice", Failing: []FailingLanding{{Bead: "gt-twice", Stage: "push", Failures: 2, NextTry: ago(-4 * time.Minute), Error: "the pre-push hook refused land/gt-twice"}}},
	}
	r := Compute(context.Background(), inputs(f))
	if got := field(t, r, "landing/once"); got.Verdict != Green {
		t.Errorf("landing/once = %+v, want green: one failure is retried", got)
	}
	got := field(t, r, "landing/twice")
	if got.Verdict != Degraded {
		t.Fatalf("landing/twice = %+v, want degraded", got)
	}
	if got.Value != "0 pending, 1 failing" {
		t.Errorf("landing/twice value = %q, want the failing count", got.Value)
	}
	for _, want := range []string{"gt-twice", "2 times in a row", "push", "next try in 4m", "the pre-push hook refused land/gt-twice"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("landing/twice detail %q; want it to name %q", got.Detail, want)
		}
	}
	if got.Tag != Recorded {
		t.Errorf("landing/twice tag = %s, want RECORDED", got.Tag)
	}
}

// A failing landing never lowers a verdict the wait limits already raised:
// the field is the worse of the two.
func TestLandingFieldKeepsTheWorseOfWaitAndFailure(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.landings = []RigLandings{{
		Rig: "stuck", Pending: 1, Oldest: ago(LandingWaitBudget + time.Minute), OldestBead: "gt-stuck",
		Failing: []FailingLanding{{Bead: "gt-failing", Stage: "ci", Failures: 3, Error: "no verdict"}},
	}}
	got := field(t, Compute(context.Background(), inputs(f)), "landing/stuck")
	if got.Verdict != Red {
		t.Fatalf("landing/stuck = %+v, want the wait's red", got)
	}
	for _, want := range []string{"gt-stuck", "gt-failing"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q; want both facts, missing %q", got.Detail, want)
		}
	}
}

// A rig with nothing failing renders exactly as it did before the failing
// landings were read: the field is a queue's, and its value, detail and
// verdict are untouched (gt-fn9e6.44).
func TestLandingFieldUnchangedWithNothingFailing(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.landings = []RigLandings{{Rig: "quiet", Landed: 3, Pending: 0}}
	got := field(t, Compute(context.Background(), inputs(f)), "landing/quiet")
	want := Field{Name: FieldLanding, Rig: "quiet", Tag: Recorded, Verdict: Green, Value: "0 pending"}
	if got != want {
		t.Fatalf("landing/quiet = %+v, want %+v", got, want)
	}

	// And the same rig once its failing landing lands: the worker's snapshot
	// carries nothing, so the field returns to what it was.
	f.landings = append(f.landings, RigLandings{Rig: "recovered", Pending: 1, Oldest: ago(time.Minute), OldestBead: "gt-x"})
	before := field(t, Compute(context.Background(), inputs(f)), "landing/recovered")
	f.landings[len(f.landings)-1].Failing = []FailingLanding{{Bead: "gt-x", Stage: "push", Failures: 2, NextTry: ago(-time.Minute), Error: "refused"}}
	during := field(t, Compute(context.Background(), inputs(f)), "landing/recovered")
	if during.Verdict != Degraded {
		t.Fatalf("landing/recovered = %+v, want degraded while failing", during)
	}
	f.landings[len(f.landings)-1].Failing = nil
	after := field(t, Compute(context.Background(), inputs(f)), "landing/recovered")
	if after != before {
		t.Fatalf("landing/recovered = %+v after the landing, want %+v", after, before)
	}
}
