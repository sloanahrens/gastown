package cmd

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// contentionErr is a bond lost to a Dolt serialization failure.
func contentionErr() error {
	return errors.New("formula failed: bd mol bond: Error 1213 (40001): serialization failure")
}

// A bond lost on every retry is a deferral that leaves its schedule on the
// bead: attempt one, dated now, retried five seconds later. Nothing labels the
// bead, so the queue keeps it (gt-kmkpao).
func TestSpecContentionRecordALostBondIsRecordedAndDeferred(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	for i := 0; i < specdispatch.RetryAttempts; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contentionErr())
	}
	r := runSpecDispatchCycle(f.env())

	if len(f.slung) != 0 || len(r.Deferred) != 1 || len(r.Failed) != 0 {
		t.Fatalf("slung %v deferred %+v failed %+v; want one deferral and no sling", f.slung, r.Deferred, r.Failed)
	}
	if line := r.Deferred[0].Line; !strings.Contains(line, "database contention") || !strings.Contains(line, "(attempt 1)") {
		t.Errorf("deferral line = %q, want the contention attempt named", line)
	}
	rec, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata)
	if !ok {
		t.Fatalf("metadata %q holds no contention record, want one", f.specs["gt-a"].Metadata)
	}
	if rec.Attempts != 1 || !rec.First.Equal(specTestNow) || !rec.RetryAt.Equal(specTestNow.Add(5*time.Second)) {
		t.Errorf("record = %+v, want attempt 1 from %v retried at %v", rec, specTestNow, specTestNow.Add(5*time.Second))
	}
	if got := f.labels["gt-a"]; len(got) != 0 {
		t.Errorf("labels = %v, want none: contention is the host's load, not the bead's defect", got)
	}
	if n := len(f.notes["gt-a"]); n != 0 {
		t.Errorf("annotated %d times, want none", n)
	}
}

// A sling that dispatches the bead ends the contended run, so the schedule
// that would hold it out of the queue is cleared (gt-kmkpao).
func TestSpecContentionRecordASuccessfulSlingClearsTheSchedule(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = contentionMetadata(specTestNow.Add(-20*time.Second), 2, specTestNow.Add(-5*time.Second))
	f := newFakeSpecTown(spec)
	r := runSpecDispatchCycle(f.env())

	if len(r.Dispatched) != 1 || len(f.slung) != 1 {
		t.Fatalf("dispatched %+v slung %v, want the bead slung", r.Dispatched, f.slung)
	}
	if rec, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata); ok {
		t.Errorf("metadata %q still carries record %+v, want the schedule cleared", f.specs["gt-a"].Metadata, rec)
	}
}

// A schedule that cannot be written is reported in the tick, and the bead is
// deferred and unlabeled: the loss is the host's, so a person has nothing to
// clear (gt-kmkpao).
func TestSpecContentionRecordAFailedWriteIsReportedAndNotLabeled(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.contentionWriteErr["gt-a"] = errors.New("bd update: boom")
	for i := 0; i < specdispatch.RetryAttempts; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contentionErr())
	}
	r := runSpecDispatchCycle(f.env())

	if len(r.Deferred) != 1 || len(r.Failed) != 0 {
		t.Fatalf("deferred %+v failed %+v, want one deferral", r.Deferred, r.Failed)
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "recording the contention retry") || !strings.Contains(r.Errors[0], "bd update: boom") {
		t.Errorf("errors = %v, want one naming the failed schedule write", r.Errors)
	}
	if got := f.labels["gt-a"]; len(got) != 0 {
		t.Errorf("labels = %v, want none: a lost schedule is reported, not held for a person", got)
	}
	if rec, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata); ok {
		t.Errorf("metadata %q carries record %+v, want the failed write to leave it empty", f.specs["gt-a"].Metadata, rec)
	}
}

// A background sling continues the schedule the tick read: the record rides
// the job through the wire, so a loss in the job is the run's next attempt
// rather than a fresh bead's first (gt-kmkpao).
func TestSpecContentionRecordBackgroundSlingCarriesTheSchedule(t *testing.T) {
	t.Parallel()
	first := specTestNow.Add(-20 * time.Second)
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = contentionMetadata(first, 1, specTestNow.Add(-5*time.Second))
	f := newFakeSpecTown(spec)
	for i := 0; i < specdispatch.RetryAttempts; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contentionErr())
	}

	planned := runSpecDispatchCycle(backgroundEnv(f))
	if len(planned.Pending) != 1 || planned.Pending[0].Contention == nil || planned.Pending[0].Contention.Attempts != 1 {
		t.Fatalf("pending = %+v, want one job carrying attempt 1", planned.Pending)
	}
	raw, err := json.Marshal(planned)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct{ Pending []specSlingJob }
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Pending) != 1 {
		t.Fatalf("report did not carry the job: %v %s", err, raw)
	}
	if wire.Pending[0].Contention == nil || wire.Pending[0].Contention.Attempts != 1 {
		t.Fatalf("job on the wire = %+v, want the schedule carried", wire.Pending[0])
	}

	report := runSpecSlingJob(f.env(), wire.Pending[0])
	if len(report.Deferred) != 1 {
		t.Fatalf("sling report = %+v, want the loss deferred", report)
	}
	rec, ok := specdispatch.ContentionRecord(f.specs["gt-a"].Metadata)
	if !ok || rec.Attempts != 2 || !rec.First.Equal(first) {
		t.Errorf("record = %+v (ok %v), want attempt 2 from %v", rec, ok, first)
	}
}

// A tick with no schedule writer cannot record the loss, so it says so:
// dropping it would leave the next tick nothing to add to, and labeling the
// bead would hold it for a person over the host's load (gt-kmkpao).
func TestSpecContentionRecordAnUnwiredWriterIsReported(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.RecordContention = nil
	for i := 0; i < specdispatch.RetryAttempts; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contentionErr())
	}
	r := runSpecDispatchCycle(env)

	if len(r.Deferred) != 1 || len(r.Failed) != 0 {
		t.Fatalf("deferred %+v failed %+v, want one deferral", r.Deferred, r.Failed)
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "no contention store is wired") {
		t.Errorf("errors = %v, want one naming the unwired store", r.Errors)
	}
	if got := f.labels["gt-a"]; len(got) != 0 {
		t.Errorf("labels = %v, want none", got)
	}
}

// contentionMetadata is a bead's metadata carrying the three contention keys.
func contentionMetadata(first time.Time, attempts int, retryAt time.Time) []byte {
	return []byte(`{"` + specdispatch.ContentionFirstKey + `":"` + first.UTC().Format(time.RFC3339) +
		`","` + specdispatch.ContentionAttemptsKey + `":` + strconv.Itoa(attempts) +
		`,"` + specdispatch.ContentionRetryAtKey + `":"` + retryAt.UTC().Format(time.RFC3339) + `"}`)
}
