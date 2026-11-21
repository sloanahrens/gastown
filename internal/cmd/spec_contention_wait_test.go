package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// A bead whose lost bond scheduled a retry still ahead is skipped with the
// backoff line and takes no seat: the sling waits for the retry time (gt-6kg6xb).
func TestSpecContentionWaitARetryAheadIsSkippedWithNoSling(t *testing.T) {
	t.Parallel()
	retryAt := specTestNow.Add(30 * time.Second)
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = contentionMetadata(specTestNow.Add(-10*time.Second), 1, retryAt)
	f := newFakeSpecTown(spec)
	r := runSpecDispatchCycle(f.env())

	if len(f.slung) != 0 || len(r.Dispatched) != 0 {
		t.Fatalf("slung %v dispatched %+v, want no sling while the retry is ahead", f.slung, r.Dispatched)
	}
	want := "gt-a: " + specdispatch.SkipContentionBackoff + retryAt.Format(time.RFC3339)
	if len(r.Skipped) != 1 || r.Skipped[0].Line != want {
		t.Errorf("skipped = %+v, want one line %q", r.Skipped, want)
	}
}

// The same bead is slung once the tick clock passes its retry time.
func TestSpecContentionWaitTheBeadIsSlungOnceTheRetryPasses(t *testing.T) {
	t.Parallel()
	retryAt := specTestNow.Add(30 * time.Second)
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = contentionMetadata(specTestNow.Add(-10*time.Second), 1, retryAt)
	f := newFakeSpecTown(spec)
	env := f.env()
	now := specTestNow
	env.Now = func() time.Time { return now }

	runSpecDispatchCycle(env)
	if len(f.slung) != 0 {
		t.Fatalf("slung %v before the retry time, want none", f.slung)
	}

	now = retryAt
	r := runSpecDispatchCycle(env)
	if len(f.slung) != 1 || f.slung[0] != "gt-a" || len(r.Skipped) != 0 {
		t.Fatalf("slung %v skipped %+v at the retry time, want gt-a slung", f.slung, r.Skipped)
	}
}

// A bead with no contention keys is slung as it always was, and metadata that
// holds other keys is no schedule.
func TestSpecContentionWaitABeadWithNoScheduleIsSlung(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = []byte(`{"unrelated":"x"}`)
	f := newFakeSpecTown(spec)
	r := runSpecDispatchCycle(f.env())

	if len(f.slung) != 1 || f.slung[0] != "gt-a" || len(r.Skipped) != 0 {
		t.Fatalf("slung %v skipped %+v, want gt-a slung with no skip", f.slung, r.Skipped)
	}
}

// A schedule that cannot be read has no retry time, so it holds nothing: the
// bead is slung, and the later beads bound the schedule (gt-6kg6xb).
func TestSpecContentionWaitAnUnreadableScheduleDoesNotHold(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = []byte(`{"` + specdispatch.ContentionFirstKey + `":"not-a-time"}`)
	f := newFakeSpecTown(spec)
	r := runSpecDispatchCycle(f.env())

	if len(f.slung) != 1 || f.slung[0] != "gt-a" || len(r.Skipped) != 0 {
		t.Fatalf("slung %v skipped %+v, want gt-a slung: an unreadable schedule holds nothing", f.slung, r.Skipped)
	}
}

// The wait is read from the fresh bead, not the candidate read: a loss written
// between the two reads holds the bead.
func TestSpecContentionWaitTheFreshReadHolds(t *testing.T) {
	t.Parallel()
	retryAt := specTestNow.Add(30 * time.Second)
	// The candidate board read is clean; the loss lands before the fresh read,
	// which is the first Show call for this bead.
	clean := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	waiting := clean
	waiting.Metadata = contentionMetadata(specTestNow.Add(-10*time.Second), 1, retryAt)
	f := newFakeSpecTown(clean)
	f.showSeq["gt-a"] = []specRead{{spec: waiting}}
	r := runSpecDispatchCycle(f.env())

	if len(f.slung) != 0 || len(r.Skipped) != 1 {
		t.Fatalf("slung %v skipped %+v, want the fresh read's schedule to hold the bead", f.slung, r.Skipped)
	}
}

// A waiting bead takes no seat, so the bead behind it on the same one-slot seat
// is slung.
func TestSpecContentionWaitAWaitingBeadTakesNoSeat(t *testing.T) {
	t.Parallel()
	pinned := func(id string) specdispatch.Spec {
		s := cleanSpec(id, 1, "2026-09-29T10:00:00Z")
		s.Labels = []string{"agent:deepseek-flash"}
		return s
	}
	resting := pinned("gt-resting")
	resting.Metadata = contentionMetadata(specTestNow.Add(-10*time.Second), 1, specTestNow.Add(time.Minute))
	f := newFakeSpecTown(resting, pinned("gt-same"))
	f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 1}}
	r := runSpecDispatchCycle(f.env())

	if len(f.slung) != 1 || f.slung[0] != "gt-same" {
		t.Fatalf("slung %v, want only gt-same: the waiting bead must not take the seat", f.slung)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Bead != "gt-resting" {
		t.Fatalf("skipped = %+v, want only gt-resting", r.Skipped)
	}
}

// A dry run predicts the live tick's hold (gt-qjlu07): a bead whose contention
// retry is still ahead is reported as held with its backoff time, not as
// would-sling.
func TestSpecContentionWaitDryRun(t *testing.T) {
	t.Parallel()
	retryAt := specTestNow.Add(30 * time.Second)
	spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	spec.Metadata = contentionMetadata(specTestNow.Add(-10*time.Second), 1, retryAt)
	f := newFakeSpecTown(spec)
	env := f.env()
	env.DryRun = true
	r := runSpecDispatchCycle(env)

	if len(r.Dispatched) != 0 || len(f.slung) != 0 {
		t.Fatalf("dispatched %+v slung %v, want the waiting bead reported as held", r.Dispatched, f.slung)
	}
	want := "gt-a: " + specdispatch.SkipContentionBackoff + retryAt.UTC().Format(time.RFC3339)
	if len(r.Skipped) != 1 || r.Skipped[0].Line != want {
		t.Fatalf("skipped = %+v, want one line %q", r.Skipped, want)
	}
}

// A dry run's re-read can fail; it then reports the bead as held, as the live
// tick's failed re-read does, rather than as would-sling (gt-qjlu07).
func TestSpecContentionWaitDryRunRereadFailureHolds(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.DryRun = true
	env.ShowFresh = func(string) (specdispatch.Spec, error) {
		return specdispatch.Spec{}, errors.New("bd show: boom")
	}
	r := runSpecDispatchCycle(env)

	if len(r.Dispatched) != 0 || len(f.slung) != 0 {
		t.Fatalf("dispatched %+v, want the unreadable bead held", r.Dispatched)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "re-read failed") {
		t.Fatalf("skipped = %+v, want one re-read-failed hold", r.Skipped)
	}
}
