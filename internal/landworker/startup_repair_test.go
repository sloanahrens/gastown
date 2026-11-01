package landworker

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// repairBeads is how many distinct beads the startup fixture's records name.
const repairBeads = 40

func repairBeadID(i int) string { return fmt.Sprintf("gt-rep%02d", i) }

// batchStore is a worker's store that records the reads the startup repair
// makes: every batch by its ids, every Show by its id. batchErr, when set,
// fails each batch in place of reading it.
type batchStore struct {
	Beads
	fake     *beadsfake.Fake
	shows    []string
	batches  [][]string
	batchErr error
}

func (s *batchStore) Show(id string) (*beads.Issue, error) {
	s.shows = append(s.shows, id)
	return s.fake.Show(id)
}

func (s *batchStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	s.batches = append(s.batches, slices.Clone(ids))
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	return s.fake.ShowMultiple(ids)
}

// perRecordStore is a worker's store without ShowMultiple: the interface it
// embeds carries none, so the worker reads one record at a time as it did
// before the batch.
type perRecordStore struct {
	Beads
	fake  *beadsfake.Fake
	shows []string
}

func (s *perRecordStore) Show(id string) (*beads.Issue, error) {
	s.shows = append(s.shows, id)
	return s.fake.Show(id)
}

// repairFixture seeds a rig whose landings file holds n records spread over
// repairBeads beads. Every tenth bead is missing from the store and every
// tenth bead after that is closed; every eleventh record's commit is not on
// the target. routeOf names each record's route, and nil records one from
// this worker.
func repairFixture(t *testing.T, n int, routeOf func(j int) string) (*harness, []land.LandingRecord) {
	t.Helper()
	h := newHarness(t)
	recs := make([]land.LandingRecord, 0, n)
	for j := 0; j < n; j++ {
		rec := land.LandingRecord{
			BeadID:       repairBeadID(j % repairBeads),
			Rig:          "gastown",
			Branch:       branch,
			Head:         noteHead,
			Target:       "main",
			LandedCommit: fmt.Sprintf("%040x", j+1),
		}
		if routeOf != nil {
			rec.Route = routeOf(j)
		}
		recs = append(recs, rec)
		h.remote.contains[rec.LandedCommit] = j%11 != 0
	}
	h.files.recs = recs
	for i := 0; i < repairBeads; i++ {
		status := "hooked"
		switch i % 10 {
		case 0:
			continue // no such bead: the read finds nothing
		case 5:
			status = "closed"
		}
		h.bd.Seed(beads.Issue{ID: repairBeadID(i), Title: "landed work", Status: status, Type: "task", Assignee: "gastown/polecats/opal"})
	}
	return h, recs
}

// queueRepairs runs the startup repair of a fixture whose worker reads
// through store.
func queueRepairs(t *testing.T, h *harness, store Beads) map[string]land.Work {
	t.Helper()
	h.w.Beads = store
	h.w.pendingRepair = map[string]land.Work{}
	h.w.queueRecentRepairs()
	return h.w.pendingRepair
}

func TestStartupRepairReadsEachWorkersRecordsInOneCall(t *testing.T) {
	t.Parallel()
	const records = 50
	h, recs := repairFixture(t, records, nil)
	store := &batchStore{Beads: h.bd, fake: h.bd}
	pending := queueRepairs(t, h, store)

	if len(store.batches) != 1 {
		t.Fatalf("ShowMultiple calls = %d, want 1 for %d records", len(store.batches), len(recs))
	}
	if len(store.shows) != 0 {
		t.Fatalf("Show calls = %v, want none: every record's bead comes from the batch", store.shows)
	}
	var wantIDs []string
	for _, rec := range recs {
		if !slices.Contains(wantIDs, rec.BeadID) {
			wantIDs = append(wantIDs, rec.BeadID)
		}
	}
	if len(wantIDs) != repairBeads {
		t.Fatalf("the fixture names %d distinct beads, want %d", len(wantIDs), repairBeads)
	}
	if got := store.batches[0]; !slices.Equal(got, wantIDs) {
		t.Fatalf("ShowMultiple ids = %v, want the records' beads in file order %v", got, wantIDs)
	}
	if len(pending) == 0 {
		t.Fatal("no repair queued: the fixture exercises nothing")
	}

	// The per-record worker queues the same repairs from the same file.
	per, _ := repairFixture(t, records, nil)
	perStore := &perRecordStore{Beads: per.bd, fake: per.bd}
	perPending := queueRepairs(t, per, perStore)
	if !maps.Equal(pending, perPending) {
		t.Fatalf("batched repairs = %v, want the per-record repairs %v", pending, perPending)
	}
}

func TestStartupRepairFallsBackToPerRecordReads(t *testing.T) {
	t.Parallel()
	const records = 50
	const crewRecord = 7
	routeOf := func(j int) string {
		if j == crewRecord {
			return "crew" // another worker's landing, which this worker never reads
		}
		return "daemon"
	}
	h, recs := repairFixture(t, records, routeOf)
	store := &batchStore{Beads: h.bd, fake: h.bd, batchErr: errors.New("bd show: context deadline exceeded")}
	pending := queueRepairs(t, h, store)

	if len(store.batches) != 1 {
		t.Fatalf("ShowMultiple calls = %d, want the one that failed", len(store.batches))
	}
	var wantShows []string
	for j, rec := range recs {
		if j != crewRecord {
			wantShows = append(wantShows, rec.BeadID)
		}
	}
	if !slices.Equal(store.shows, wantShows) {
		t.Fatalf("Show calls = %v, want one per record of this worker's %v", store.shows, wantShows)
	}
	// A bead the batch does not return reads as a Show that found nothing.
	if _, ok := pending[repairBeadID(0)]; ok {
		t.Fatalf("%s is not in the store, yet a repair was queued for it", repairBeadID(0))
	}

	// The per-record worker queues the same repairs from the same file.
	per, _ := repairFixture(t, records, routeOf)
	perStore := &perRecordStore{Beads: per.bd, fake: per.bd}
	perPending := queueRepairs(t, per, perStore)
	if !maps.Equal(pending, perPending) {
		t.Fatalf("repairs after a failed batch = %v, want the per-record repairs %v", pending, perPending)
	}
}

// flakyBatch is a RetryInner whose ShowMultiple fails errs times before it
// answers from the fake.
type flakyBatch struct {
	*beadsfake.Fake
	fails int
	err   error
	calls int
}

func (b *flakyBatch) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	b.calls++
	if b.calls <= b.fails {
		return nil, b.err
	}
	return b.Fake.ShowMultiple(ids)
}

func TestRetryBeadsShowMultiple(t *testing.T) {
	t.Parallel()
	serialization := errors.New("bd show: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction")
	t.Run("retries a serialization failure then answers", func(t *testing.T) {
		t.Parallel()
		fake := beadsfake.New(beadsfake.WithPrefix("gt"))
		fake.Seed(beads.Issue{ID: "gt-a", Title: "a", Type: "task"})
		fake.Seed(beads.Issue{ID: "gt-b", Title: "b", Type: "task"})
		inner := &flakyBatch{Fake: fake, fails: 2, err: serialization}
		var waits []time.Duration
		r := RetryBeads{Inner: inner, Sleep: func(d time.Duration) { waits = append(waits, d) }}
		got, err := r.ShowMultiple([]string{"gt-a", "gt-b", "gt-gone"})
		if err != nil {
			t.Fatalf("ShowMultiple: %v", err)
		}
		if inner.calls != 3 || len(waits) != 2 {
			t.Fatalf("calls %d waits %d; want 3 calls and 2 waits", inner.calls, len(waits))
		}
		if len(got) != 2 || got["gt-a"] == nil || got["gt-b"] == nil {
			t.Fatalf("ShowMultiple = %v, want the two beads the store holds", got)
		}
	})
	t.Run("returns another error at once", func(t *testing.T) {
		t.Parallel()
		inner := &flakyBatch{Fake: beadsfake.New(), fails: 99, err: errors.New("bd show: issue not found")}
		r := RetryBeads{Inner: inner, Sleep: func(time.Duration) { t.Fatal("slept on a non-retryable error") }}
		if _, err := r.ShowMultiple([]string{"gt-a"}); err == nil || inner.calls != 1 {
			t.Fatalf("err %v calls %d; want the error after one call", err, inner.calls)
		}
	})
	t.Run("reports an inner client that cannot batch", func(t *testing.T) {
		t.Parallel()
		// flakyBeads embeds RetryInner instead of a concrete store, so it
		// carries no ShowMultiple: the client before the batch.
		inner := &flakyBeads{RetryInner: beadsfake.New(beadsfake.WithPrefix("gt"))}
		r := RetryBeads{Inner: inner, Sleep: func(time.Duration) { t.Fatal("slept on a missing capability") }}
		if _, err := r.ShowMultiple([]string{"gt-a"}); !errors.Is(err, ErrNoShowMultiple) {
			t.Fatalf("ShowMultiple = %v, want ErrNoShowMultiple", err)
		}
	})
}

// noBatchInner is a RetryInner that hides the fake's ShowMultiple by embedding
// only the interface, and records the single reads.
type noBatchInner struct {
	RetryInner
	shows []string
}

func (n *noBatchInner) Show(id string) (*beads.Issue, error) {
	n.shows = append(n.shows, id)
	return n.RetryInner.Show(id)
}

// The production client is a RetryBeads, which always has ShowMultiple; over a
// store that cannot batch it answers ErrNoShowMultiple. The worker then reads
// each record's bead on its own, queues the repairs the per-record worker
// queues, and says so once across passes, not on every pass.
func TestStartupRepairOverARetryClientWithoutBatching(t *testing.T) {
	t.Parallel()
	const records = 50
	h, _ := repairFixture(t, records, nil)
	inner := &noBatchInner{RetryInner: h.bd}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	store := RetryBeads{Inner: inner, Sleep: func(time.Duration) {}}

	pending := queueRepairs(t, h, store)
	queueRepairs(t, h, store)

	if len(inner.shows) == 0 {
		t.Fatal("no single Show calls: the worker did not fall back to per-record reads")
	}
	said := 0
	for _, l := range logs {
		if strings.Contains(l, "cannot batch reads") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the slow-reads line was logged %d times over two passes, want once; logs %q", said, logs)
	}
	per, _ := repairFixture(t, records, nil)
	perPending := queueRepairs(t, per, &perRecordStore{Beads: per.bd, fake: per.bd})
	if !maps.Equal(pending, perPending) {
		t.Fatalf("repairs over the retry client = %v, want the per-record repairs %v", pending, perPending)
	}
}
