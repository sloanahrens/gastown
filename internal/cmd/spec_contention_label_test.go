package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// A bead whose contention outlasts the schedule is held for a person under its
// own reason beside the failed label, and the dispatcher's retry after the
// pause is dated on it (gt-nnq9a2).
func TestSpecContentionLabelALossPastTheWindowIsLabeled(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = contentionMetadata(specTestNow.Add(-15*time.Minute), 4, specTestNow.Add(-time.Minute))
	f := newFakeSpecTown(spec)
	for i := 0; i < specdispatch.RetryAttempts; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contentionErr())
	}
	r := runSpecDispatchCycle(f.env())

	if len(r.Failed) != 1 || len(r.Deferred) != 0 || len(f.slung) != 0 {
		t.Fatalf("failed %+v deferred %+v slung %v; want one failure and no sling", r.Failed, r.Deferred, f.slung)
	}
	if line := r.Failed[0].Line; !strings.Contains(line, "database contention outlasted") {
		t.Errorf("failure line = %q, want it to name the spent schedule", line)
	}
	got := f.labels["gt-a"]
	if !slices.Contains(got, specdispatch.DispatchFailedLabel) || !slices.Contains(got, specdispatch.ContentionLabel) {
		t.Errorf("labels = %v, want both %s and %s", got, specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel)
	}
	rec, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata)
	if !ok {
		t.Fatalf("metadata %q holds no contention record, want the retry dated", f.specs["gt-a"].Metadata)
	}
	if want := specTestNow.Add(specdispatch.ContentionPause); !rec.RetryAt.Equal(want) {
		t.Errorf("retry_at = %v, want %v", rec.RetryAt, want)
	}
	if rec.Attempts != 5 {
		t.Errorf("attempts = %d, want the fifth loss recorded", rec.Attempts)
	}
	notes := f.notes["gt-a"]
	if len(notes) != 1 || !strings.Contains(notes[0], "database contention outlasted") ||
		!strings.Contains(notes[0], rec.RetryAt.Format(time.RFC3339)) {
		t.Errorf("notes = %v, want one naming the spent schedule and its retry time", notes)
	}
}

// A loss inside the window is the deferral it always was: the next attempt is
// scheduled, and no label holds the bead out of the queue (gt-kmkpao).
func TestSpecContentionLabelALossInsideTheWindowIsNotLabeled(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = contentionMetadata(specTestNow.Add(-time.Minute), 1, specTestNow.Add(-time.Second))
	f := newFakeSpecTown(spec)
	for i := 0; i < specdispatch.RetryAttempts; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contentionErr())
	}
	r := runSpecDispatchCycle(f.env())

	if len(r.Failed) != 0 || len(r.Deferred) != 1 || len(f.slung) != 0 {
		t.Fatalf("failed %+v deferred %+v slung %v; want one deferral and no failure", r.Failed, r.Deferred, f.slung)
	}
	if got := f.labels["gt-a"]; len(got) != 0 {
		t.Errorf("labels = %v, want none inside the window", got)
	}
	rec, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata)
	if !ok {
		t.Fatalf("metadata %q holds no contention record, want the next attempt", f.specs["gt-a"].Metadata)
	}
	if want := specTestNow.Add(15 * time.Second); !rec.RetryAt.Equal(want) || rec.Attempts != 2 {
		t.Errorf("record = %+v, want attempt 2 retried at %v", rec, want)
	}
}

// A failure of another kind ends a contended run: the contention label and the
// schedule go, and the failed label is what a person clears (gt-nnq9a2).
func TestSpecContentionLabelALaterFailureDropsTheContentionState(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Labels = append(spec.Labels, specdispatch.ContentionLabel)
	spec.Metadata = contentionMetadata(specTestNow.Add(-20*time.Minute), 5, specTestNow.Add(-time.Minute))
	f := newFakeSpecTown(spec)
	f.slingErrs["gt-a"] = []error{errors.New("starting polecat session: tmux: no server")}
	env := f.env()
	env.DropContention = f.dropContention
	r := runSpecDispatchCycle(env)

	if len(r.Failed) != 1 || len(r.Errors) != 0 {
		t.Fatalf("failed %+v errors %v, want one failure and no error", r.Failed, r.Errors)
	}
	got := f.specs["gt-a"].Labels
	if !slices.Contains(got, specdispatch.DispatchFailedLabel) {
		t.Errorf("labels = %v, want the failed label", got)
	}
	if slices.Contains(got, specdispatch.ContentionLabel) {
		t.Errorf("labels = %v, want the contention label gone", got)
	}
	if rec, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata); ok {
		t.Errorf("metadata %q carries record %+v, want the schedule dropped", f.specs["gt-a"].Metadata, rec)
	}
}

// A drop that cannot be written is reported, and the contention label and its
// schedule stay: the bead waits for a person, and the tick's log says why
// (gt-nnq9a2).
func TestSpecContentionLabelAFailedDropIsReported(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Labels = append(spec.Labels, specdispatch.ContentionLabel)
	spec.Metadata = contentionMetadata(specTestNow.Add(-20*time.Minute), 5, specTestNow.Add(-time.Minute))
	f := newFakeSpecTown(spec)
	f.slingErrs["gt-a"] = []error{errors.New("starting polecat session: tmux: no server")}
	env := f.env()
	env.DropContention = func(string) error { return errors.New("bd update: dolt: connection refused") }
	r := runSpecDispatchCycle(env)

	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "dropping the contention retry") ||
		!strings.Contains(r.Errors[0], "connection refused") {
		t.Errorf("errors = %v, want one naming the failed drop", r.Errors)
	}
	got := f.specs["gt-a"].Labels
	if !slices.Contains(got, specdispatch.DispatchFailedLabel) || !slices.Contains(got, specdispatch.ContentionLabel) {
		t.Errorf("labels = %v, want the couple left standing after the failed drop", got)
	}
	if _, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata); !ok {
		t.Errorf("metadata %q dropped its schedule, want the failed drop to leave it", f.specs["gt-a"].Metadata)
	}
}
