package metrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// reviewTestTown is the town root every review test reads under.
const reviewTestTown = "/town"

// reviewKindColumnNames are the four author-kind columns a collection
// publishes beside the counts.
var reviewKindColumnNames = []string{
	"om_polecat_reviews_1h",
	"om_crew_reviews_1h",
	"om_overseer_reviews_1h",
	"om_unknown_reviews_1h",
}

// reviewTestEnv serves the review log the test names: one file's content, no
// file at all, or the error a read gets instead.
func reviewTestEnv(t *testing.T, content string, missing bool, tailErr error) Env {
	t.Helper()
	path := land.TownReviewsFile(reviewTestTown).Path
	env := envAt(fixedNow())
	env.Town = reviewTestTown
	env.Tail = func(got string, maxBytes int64) ([]byte, error) {
		if got != path {
			t.Errorf("Tail(%s), want %s", got, path)
		}
		if maxBytes != sourceLogTailBytes {
			t.Errorf("Tail(%s) read %d bytes, want the %d-byte bound", got, maxBytes, sourceLogTailBytes)
		}
		if tailErr != nil {
			return nil, tailErr
		}
		if missing {
			return nil, fs.ErrNotExist
		}
		return []byte(content), nil
	}
	return env
}

// reviewLine is one verdict line in the shape land.ReviewRecord decodes.
func reviewLine(bead, verdict string, attempt int, score float64, rejected bool, at time.Time) string {
	return fmt.Sprintf(`{"bead":%q,"rig":"gastown","attempt":%d,"score":%g,"verdict":%q,"rejected":%t,"findings":2,"bar":7,"at":%q}`,
		bead, attempt, score, verdict, rejected, at.UTC().Format(time.RFC3339))
}

// reviewStoreFake is the bead store the review group reads in these tests: the
// notes it holds for a bead, the error a bead's read gets, a bead whose read
// waits for its deadline to pass, and a record of every read, so a test sees
// which directory's store each bead was read from and under what deadline.
type reviewStoreFake struct {
	notes    map[string]string // bead id -> the notes its store holds
	assignee map[string]string // bead id -> the assignee its store answers
	fail     map[string]error  // bead id -> the error its read gets
	slow     string            // a bead whose read waits for its deadline

	mu              sync.Mutex
	reads           []reviewStoreRead
	deadlineMissing bool
}

// reviewStoreRead is one read of the store: the directory it was opened for,
// the ids it was asked, when it started, and the deadline it was given.
type reviewStoreRead struct {
	dir         string
	ids         []string
	at          time.Time
	deadline    time.Time
	hasDeadline bool
}

// seam is the store seam the review group reads through, one store per
// directory the review names.
func (f *reviewStoreFake) seam() BeadStores {
	return func(ctx context.Context, dir string) BeadStore {
		deadline, ok := ctx.Deadline()
		if !ok {
			f.mu.Lock()
			f.deadlineMissing = true
			f.mu.Unlock()
		}
		return &reviewFakeStore{f: f, dir: dir, ctx: ctx, deadline: deadline, hasDeadline: ok}
	}
}

// readIDs lists every id the store was asked, read order included.
func (f *reviewStoreFake) readIDs() []string {
	var ids []string
	for _, read := range f.reads {
		ids = append(ids, read.ids...)
	}
	return ids
}

// reviewFakeStore is one directory's store as the source opened it: the
// directory, and the context the source gave the factory, which is the
// deadline a real store's read runs under.
type reviewFakeStore struct {
	f           *reviewStoreFake
	dir         string
	ctx         context.Context
	deadline    time.Time
	hasDeadline bool
}

func (s *reviewFakeStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the review test read the board")
}

func (s *reviewFakeStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the review test read the ready queue")
}

func (s *reviewFakeStore) Blocked() ([]*beads.Issue, error) {
	return nil, errors.New("the review test read the blocked list")
}

func (s *reviewFakeStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	f := s.f
	f.mu.Lock()
	f.reads = append(f.reads, reviewStoreRead{
		dir:         s.dir,
		ids:         append([]string(nil), ids...),
		at:          time.Now(),
		deadline:    s.deadline,
		hasDeadline: s.hasDeadline,
	})
	f.mu.Unlock()
	// A read whose deadline has gone fails, as bd's call under it does.
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		if id == f.slow {
			// A store whose read hangs: only the deadline the source gave it
			// ends the read, so a test that passes no deadline must not hang.
			if !s.hasDeadline {
				return nil, errors.New("a slow read was given no deadline to end it")
			}
			<-s.ctx.Done()
			return nil, s.ctx.Err()
		}
		if err := f.fail[id]; err != nil {
			return nil, err
		}
		notes, ok := f.notes[id]
		if !ok {
			continue // a bead the store no longer holds is left out, not refused
		}
		out[id] = &beads.Issue{ID: id, Notes: notes, Assignee: f.assignee[id]}
	}
	return out, nil
}

// reviewBeadNotes is the notes a reviewed bead carries: the Branch line the
// attribution reads, or none.
func reviewBeadNotes(branch string) string {
	if branch == "" {
		return "READY TO LAND\nHead: abc\n"
	}
	return "READY TO LAND\nBranch: " + branch + "\nHead: abc\n"
}

// reviewNotFoundEnvelope is bd's machine-mode answer to a show that resolved no
// id: data null, exit 20, the missing id under error.ids (the shape captured
// from bd on this host; internal/beads' own tests carry the two-id copy).
const reviewNotFoundEnvelope = `{
  "schema_version": 1,
  "contract_version": 1,
  "data": null,
  "pagination": null,
  "error": {
    "kind": "not_found",
    "message": "1 of 1 ids failed",
    "ids": [
      {"id": "gt-gone", "kind": "not_found", "message": "no issue found matching \"gt-gone\""}
    ]
  }
}`

func TestSourceReviewCountsTheLastHour(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-50*time.Minute)) + "\n" +
		reviewLine("gt-a2", land.VerdictRequestChanges, 1, 4, false, now.Add(-40*time.Minute)) + "\n" +
		// An approving verdict the landing refused is a rejection too.
		reviewLine("gt-a3", land.VerdictApprove, 2, 6, true, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-a4", land.VerdictApprove, 3, 9, false, now.Add(-20*time.Minute)) + "\n" +
		reviewLine("gt-old", land.VerdictRequestChanges, 1, 1, false, now.Add(-90*time.Minute)) + "\n" + // outside the hour
		reviewLine("gt-future", land.VerdictApprove, 1, 10, false, now.Add(5*time.Minute)) + "\n" + // not yet
		`{"bead":"gt-torn","verdict":` + "\n" + // torn append
		`{"verdict":"approve","at":"2026-10-24T12:30:00Z"}` + "\n" + // no bead
		`{"bead":"gt-nv","at":"2026-10-24T12:30:00Z"}` + "\n" // no verdict

	store := &reviewStoreFake{notes: map[string]string{
		"gt-a1": reviewBeadNotes("polecat/opal/gt-a1+mv1"),
		"gt-a2": reviewBeadNotes("crew/merlin/gt-a2"),
		"gt-a3": reviewBeadNotes("crew/overseer/gt-a3"),
		"gt-a4": reviewBeadNotes(""),
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Beads = store.seam()
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          4,
		"om_rejections_1h":       2,
		"om_mean_score_1h":       6.875,
		"om_second_attempts_1h":  2,
		"om_polecat_reviews_1h":  1,
		"om_crew_reviews_1h":     1,
		"om_overseer_reviews_1h": 1,
		"om_unknown_reviews_1h":  1,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v", cols, want)
	}
	if store.deadlineMissing {
		t.Error("a store read ran without a deadline")
	}
	// Each review bead is read once, from its review's rig, one id per read.
	if len(store.reads) != 4 {
		t.Fatalf("store reads = %d, want one per reviewed bead", len(store.reads))
	}
	for _, read := range store.reads {
		if read.dir != filepath.Join(reviewTestTown, "gastown") {
			t.Errorf("read %v from %q, want the review's rig", read.ids, read.dir)
		}
		if len(read.ids) != 1 {
			t.Errorf("read %v, want one bead per read", read.ids)
		}
	}
}

// A bead read that fails leaves the kind columns null, because the hour's
// reviews were not attributed; the counts the source can still make stand. A
// read the store answered with the deadline it was given is a failed read too,
// so one slow bead costs the kinds rather than hanging the group.
func TestSourceReviewAFailedBeadReadLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-a2", land.VerdictApprove, 1, 8.5, false, now.Add(-20*time.Minute)) + "\n"
	tests := []struct {
		name string
		fail error
	}{
		{name: "the store refuses the read", fail: errors.New("the store has no Dolt server")},
		{name: "the read ran out of its deadline", fail: context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &reviewStoreFake{
				notes: map[string]string{"gt-a1": reviewBeadNotes("polecat/opal/gt-a1")},
				fail:  map[string]error{"gt-a2": tc.fail},
			}
			env := reviewTestEnv(t, content, false, nil)
			env.Beads = store.seam()
			cols, err := reviewSource{}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if cols["om_reviews_1h"] != 2 {
				t.Errorf("om_reviews_1h = %v, want the 2 reviews it could count", cols["om_reviews_1h"])
			}
			for _, column := range reviewKindColumnNames {
				if value, ok := cols[column]; !ok || value != nil {
					t.Errorf("%s = %v, want null: a review it could not attribute is not an unknown author's", column, value)
				}
			}
		})
	}
}

// Two reviews of one bead are one bead read, and a review that names no rig is
// read from the town root, where bd routes the bead's prefix to its rig.
func TestSourceReviewReadsEachBeadOnce(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictRequestChanges, 1, 4, false, now.Add(-40*time.Minute)) + "\n" +
		fmt.Sprintf(`{"bead":"gt-a1","rig":"gastown","attempt":2,"score":8,"verdict":"approve","at":%q}`,
			now.Add(-20*time.Minute).UTC().Format(time.RFC3339)) + "\n" +
		fmt.Sprintf(`{"bead":"gt-a2","attempt":1,"score":8,"verdict":"approve","at":%q}`,
			now.Add(-10*time.Minute).UTC().Format(time.RFC3339)) + "\n"
	store := &reviewStoreFake{notes: map[string]string{
		"gt-a1": reviewBeadNotes("polecat/opal/gt-a1+mv1"),
		"gt-a2": reviewBeadNotes("polecat/opal/gt-a2+mv1"),
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Beads = store.seam()
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_polecat_reviews_1h"] != 3 {
		t.Errorf("om_polecat_reviews_1h = %v, want all three reviews", cols["om_polecat_reviews_1h"])
	}
	if len(store.reads) != 2 {
		t.Fatalf("store reads = %v, want one per bead, not one per review", store.reads)
	}
	dirs := map[string]string{store.reads[0].ids[0]: store.reads[0].dir, store.reads[1].ids[0]: store.reads[1].dir}
	if dirs["gt-a1"] != filepath.Join(reviewTestTown, "gastown") {
		t.Errorf("gt-a1 read from %q, want its review's rig", dirs["gt-a1"])
	}
	if dirs["gt-a2"] != reviewTestTown {
		t.Errorf("gt-a2 read from %q, want the town root for a review that names no rig", dirs["gt-a2"])
	}
}

// A crew branch whose bead the overseer holds is still the crew branch's: the
// kind is read from the branch, written once, and not from the assignee, which
// dispatch and takeover change after the review.
func TestSourceReviewReadsTheKindFromTheBranchNotTheAssignee(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8, false, now.Add(-30*time.Minute)) + "\n"
	store := &reviewStoreFake{
		notes:    map[string]string{"gt-a1": reviewBeadNotes("crew/merlin/gt-a1")},
		assignee: map[string]string{"gt-a1": "town/crew/overseer"},
	}
	env := reviewTestEnv(t, content, false, nil)
	env.Beads = store.seam()
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_crew_reviews_1h"] != 1 || cols["om_overseer_reviews_1h"] != 0 {
		t.Errorf("crew, overseer = %v, %v; want the crew branch's kind, not the bead's assignee",
			cols["om_crew_reviews_1h"], cols["om_overseer_reviews_1h"])
	}
}

// A rig that is not one path element under the town is not a directory this
// source runs bd in: the hour's kinds are null and no bd starts.
func TestSourceReviewARigOutsideTheTownLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	for _, rig := range []string{".", "..", "../gastown", "gastown/../..", "/etc", `..\..\etc`} {
		t.Run(rig, func(t *testing.T) {
			t.Parallel()
			content := fmt.Sprintf(`{"bead":"gt-a1","rig":%q,"attempt":1,"score":8,"verdict":"approve","at":%q}`,
				rig, now.Add(-30*time.Minute).UTC().Format(time.RFC3339)) + "\n"
			store := &reviewStoreFake{notes: map[string]string{
				"gt-a1": reviewBeadNotes("polecat/opal/gt-a1+mv1"),
			}}
			env := reviewTestEnv(t, content, false, nil)
			env.Beads = store.seam()
			cols, err := reviewSource{}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if cols["om_reviews_1h"] != 1 {
				t.Errorf("om_reviews_1h = %v, want the review it could count", cols["om_reviews_1h"])
			}
			for _, column := range reviewKindColumnNames {
				if value, ok := cols[column]; !ok || value != nil {
					t.Errorf("%s = %v, want null: a rig that escapes the town names no directory to read", column, value)
				}
			}
			if len(store.reads) != 0 {
				t.Errorf("store reads = %v, want none", store.reads)
			}
		})
	}
}

// The reads are sequential, so they are bounded: an hour that names more
// distinct beads than reviewBeadReadCap is not attributed at all, and none of
// it is read.
func TestSourceReviewStopsAtTheBeadReadCap(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	notes := map[string]string{}
	for i := 0; i <= reviewBeadReadCap; i++ {
		id := fmt.Sprintf("gt-c%d", i)
		notes[id] = reviewBeadNotes("polecat/opal/" + id + "+mv1")
	}
	lines := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `{"bead":"gt-c%d","rig":"gastown","attempt":1,"score":8,"verdict":"approve","at":%q}`+"\n",
				i, now.Add(-30*time.Minute).UTC().Format(time.RFC3339))
		}
		return b.String()
	}
	t.Run("at the cap", func(t *testing.T) {
		t.Parallel()
		store := &reviewStoreFake{notes: notes}
		env := reviewTestEnv(t, lines(reviewBeadReadCap), false, nil)
		env.Beads = store.seam()
		cols, err := reviewSource{}.Collect(context.Background(), env)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if cols["om_polecat_reviews_1h"] != reviewBeadReadCap {
			t.Errorf("om_polecat_reviews_1h = %v, want all %d attributed", cols["om_polecat_reviews_1h"], reviewBeadReadCap)
		}
		if len(store.reads) != reviewBeadReadCap {
			t.Errorf("store reads = %d, want one per bead at the cap", len(store.reads))
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		t.Parallel()
		store := &reviewStoreFake{notes: notes}
		env := reviewTestEnv(t, lines(reviewBeadReadCap+1), false, nil)
		env.Beads = store.seam()
		cols, err := reviewSource{}.Collect(context.Background(), env)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if cols["om_reviews_1h"] != reviewBeadReadCap+1 {
			t.Errorf("om_reviews_1h = %v, want the %d reviews it could count", cols["om_reviews_1h"], reviewBeadReadCap+1)
		}
		for _, column := range reviewKindColumnNames {
			if value, ok := cols[column]; !ok || value != nil {
				t.Errorf("%s = %v, want null: more beads than the source will read", column, value)
			}
		}
		if len(store.reads) != 0 {
			t.Errorf("store reads = %d, want none over the cap", len(store.reads))
		}
	})
}

// The per-read deadline and the cap bound one factor each; the budget bounds all
// the reads together. A budget tighter than one read is what every read runs
// under, so the reads cannot outlast it however many there are; a budget wider
// than one read leaves each read its own reviewBdTimeout deadline, so a store
// slow on one bead cannot hold the reads that follow. The test reads the
// deadline each read was given, so it waits for nothing.
func TestReviewBeadsGivesEveryReadTheSameBudgetDeadline(t *testing.T) {
	t.Parallel()
	reviews := []Review{
		{Bead: "gt-b1", Rig: "gastown"},
		{Bead: "gt-b2", Rig: "gastown"},
		{Bead: "gt-b3", Rig: "gastown"},
	}
	read := func(t *testing.T, budget time.Duration) []reviewStoreRead {
		t.Helper()
		store := &reviewStoreFake{notes: map[string]string{
			"gt-b1": reviewBeadNotes("polecat/opal/x+mv1"),
			"gt-b2": reviewBeadNotes("polecat/opal/x+mv1"),
			"gt-b3": reviewBeadNotes("polecat/opal/x+mv1"),
		}}
		env := Env{Town: t.TempDir(), Beads: store.seam()}
		rows, ok := reviewBeads(context.Background(), env, reviews, budget)
		if !ok || len(rows) != len(reviews) || len(store.reads) != len(reviews) {
			t.Fatalf("reviewBeads = %d rows, %v, %d reads, want all %d read", len(rows), ok, len(store.reads), len(reviews))
		}
		if store.deadlineMissing {
			t.Error("a read ran without a deadline")
		}
		return store.reads
	}
	t.Run("a budget tighter than one read bounds every read", func(t *testing.T) {
		t.Parallel()
		const budget = time.Second // well inside reviewBdTimeout, so only the budget can set it
		reads := read(t, budget)
		for _, got := range reads {
			// The budget's deadline is fixed before any read starts, so what is
			// left of it when a read starts is at most the budget and still
			// ahead of it; a per-read deadline would leave nearly all of
			// reviewBdTimeout.
			if left := got.deadline.Sub(got.at); left <= 0 || left > budget {
				t.Errorf("read %v's deadline is %v from its start, want what the budget (%v) leaves", got.ids, left, budget)
			}
		}
		for i, got := range reads[1:] {
			if !got.deadline.Equal(reads[0].deadline) {
				t.Errorf("read %d's deadline differs from the first's by %v, want one shared deadline", i+2, got.deadline.Sub(reads[0].deadline))
			}
		}
	})
	t.Run("a budget wider than one read gives each read its own", func(t *testing.T) {
		t.Parallel()
		reads := read(t, time.Hour)
		for _, got := range reads {
			// The deadline is fixed before the read runs, so it is at most
			// reviewBdTimeout after the read's start, and still ahead of it.
			if left := got.deadline.Sub(got.at); left <= 0 || left > reviewBdTimeout {
				t.Errorf("read %v's deadline is %v from its start, want a deadline of its own no later than reviewBdTimeout (%v)", got.ids, left, reviewBdTimeout)
			}
		}
		// A deadline shared by the directory's reads would leave every read the
		// first one's deadline; each read here is anchored at its own start.
		for i, got := range reads[1:] {
			if !got.deadline.After(reads[0].deadline) {
				t.Errorf("read %d's deadline is not after the first's: the reads share one deadline", i+2)
			}
		}
	})
}

// A budget already spent fails the attribution at its first read, so the kind
// columns are null like any other failed read. A read that hangs is ended by
// the deadline it was given: what a hung read costs is bounded by the budget
// for the whole collection, not by a deadline of the source's per-read length.
func TestReviewBeadsFailsWhenTheBudgetIsSpent(t *testing.T) {
	t.Parallel()
	t.Run("spent before the first read", func(t *testing.T) {
		t.Parallel()
		store := &reviewStoreFake{notes: map[string]string{"gt-b1": reviewBeadNotes("polecat/opal/x+mv1")}}
		env := Env{Town: t.TempDir(), Beads: store.seam()}
		rows, ok := reviewBeads(context.Background(), env, []Review{{Bead: "gt-b1", Rig: "gastown"}}, -time.Second)
		if ok || rows != nil {
			t.Errorf("reviewBeads under a spent budget = %v, %v, want a failed attribution", rows, ok)
		}
	})
	t.Run("spent while a read hangs", func(t *testing.T) {
		t.Parallel()
		const budget = 20 * time.Millisecond
		store := &reviewStoreFake{slow: "gt-b1", notes: map[string]string{"gt-b1": reviewBeadNotes("polecat/opal/x+mv1")}}
		env := Env{Town: t.TempDir(), Beads: store.seam()}
		started := time.Now()
		rows, ok := reviewBeads(context.Background(), env, []Review{{Bead: "gt-b1", Rig: "gastown"}}, budget)
		if ok || rows != nil {
			t.Errorf("reviewBeads with a hung read = %v, %v, want a failed attribution", rows, ok)
		}
		// A hung read that waited for its own reviewBdTimeout instead of the
		// budget would take at least reviewBdTimeout to answer, so the
		// constant is the bound: no tolerance, and three orders of magnitude
		// of headroom over the budget it should end at.
		if took := time.Since(started); took >= reviewBdTimeout {
			t.Errorf("the hung read took %v, want it ended at the budget (%v), not at reviewBdTimeout (%v)", took, budget, reviewBdTimeout)
		}
	})
}

// A store hung on a bead's read is ended by the deadline the read was given
// rather than by the store, so a collection still answers: the kind columns go
// null, and the counts the source can still make stand. The deadline here is
// test-sized, well inside reviewBdTimeout, since a test that waited out the
// per-bead deadline itself would take five seconds to learn the same thing.
func TestSourceReviewAHungReadEndsAtItsDeadline(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 2, 8, false, now.Add(-30*time.Minute)) + "\n"
	store := &reviewStoreFake{
		slow:  "gt-a1",
		notes: map[string]string{"gt-a1": reviewBeadNotes("polecat/opal/gt-a1+mv1")},
	}
	env := reviewTestEnv(t, content, false, nil)
	env.Beads = store.seam()
	const snapshot = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), snapshot)
	defer cancel()
	started := time.Now()
	cols, err := reviewSource{}.Collect(ctx, env)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 1 || cols["om_second_attempts_1h"] != 1 {
		t.Errorf("review = %v, want the review it could still count", cols)
	}
	for _, column := range reviewKindColumnNames {
		if value, ok := cols[column]; !ok || value != nil {
			t.Errorf("%s = %v, want null: a read that ran out of its deadline is not an unknown author's", column, value)
		}
	}
	// A read that waited for its own reviewBdTimeout instead of the deadline it
	// was given would take seconds.
	if took > reviewBdTimeout {
		t.Errorf("Collect took %v, want the read ended near its deadline (%v), not at reviewBdTimeout (%v)",
			took, snapshot, reviewBdTimeout)
	}
	if len(store.reads) != 1 {
		t.Errorf("store reads = %v, want the one hung read", store.reads)
	}
	if store.deadlineMissing {
		t.Error("the hung read ran without a deadline, so only its store could have ended it")
	}
}

// reviewBeads answers its rows in id order, so what it attributes does not
// depend on the order the review log names its beads.
func TestReviewBeadsAnswersRowsInIDOrder(t *testing.T) {
	t.Parallel()
	reviews := []Review{
		{Bead: "gt-z1", Rig: "gastown"},
		{Bead: "gt-a1", Rig: "gastown"},
		{Bead: "gt-m1", Rig: "gastown"},
	}
	store := &reviewStoreFake{notes: map[string]string{
		"gt-z1": reviewBeadNotes("polecat/opal/gt-z1+mv1"),
		"gt-a1": reviewBeadNotes("polecat/opal/gt-a1+mv1"),
		"gt-m1": reviewBeadNotes("polecat/opal/gt-m1+mv1"),
	}}
	env := Env{Town: t.TempDir(), Beads: store.seam()}
	rows, ok := reviewBeads(context.Background(), env, reviews, reviewBdBudget)
	if !ok {
		t.Fatalf("reviewBeads failed, want every bead read")
	}
	got := make([]string, len(rows))
	for i, row := range rows {
		got[i] = row.ID
	}
	if want := []string{"gt-a1", "gt-m1", "gt-z1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// A bead the store does not hold is the unknown author's rather than a failed
// read, and an id that starts with a dash is never asked of the store: bd would
// read it as a flag, so it is no bead id at all.
func TestSourceReviewAMissingBeadAndADashIDAreUnknownAuthors(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8, false, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-gone", land.VerdictApprove, 1, 8, false, now.Add(-20*time.Minute)) + "\n" +
		reviewLine("-dash", land.VerdictApprove, 1, 8, false, now.Add(-10*time.Minute)) + "\n"
	store := &reviewStoreFake{notes: map[string]string{
		"gt-a1": reviewBeadNotes("polecat/opal/gt-a1+mv1"),
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Beads = store.seam()
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          3,
		"om_rejections_1h":       0,
		"om_mean_score_1h":       8.0,
		"om_second_attempts_1h":  0,
		"om_polecat_reviews_1h":  1,
		"om_crew_reviews_1h":     0,
		"om_overseer_reviews_1h": 0,
		"om_unknown_reviews_1h":  2,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v", cols, want)
	}
	for _, id := range store.readIDs() {
		if id == "-dash" {
			t.Errorf("the store was asked for %q, which bd would read as a flag", id)
		}
	}
	if got := len(store.readIDs()); got != 2 {
		t.Errorf("store reads = %d, want the 2 beads the store could be asked for", got)
	}
}

// A reviewed bead the store no longer holds is the unknown author's, not a
// failed read: the real seam over a fake bd answers bd's not-found envelope
// (data null, exit 20) for that bead, which the store reads as none found and
// the source as an author it cannot name. The four kind columns are therefore
// filled rather than null, and the counts stand (gt-vu312, gt-55ypc).
func TestSourceReviewAMissingBeadThroughTheRealSeamIsAnUnknownAuthor(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8, false, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-gone", land.VerdictApprove, 1, 8, false, now.Add(-20*time.Minute)) + "\n"
	var asked []string
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		// The seam's probe that bd takes --allow-stale, keyed the way
		// internal/beads' own tests key it.
		if len(args) == 2 && args[0] == "--allow-stale" && args[1] == "version" {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		if len(args) == 4 && args[0] == "--allow-stale" && args[1] == "show" && args[2] == "--json" {
			asked = append(asked, args[3])
			switch args[3] {
			case "gt-a1":
				return []byte(`[{"id":"gt-a1","notes":"READY TO LAND\nBranch: polecat/opal/gt-a1\nHead: abc\n"}]`), nil, nil
			case "gt-gone":
				return []byte(reviewNotFoundEnvelope), []byte("Issue gt-gone not found\n"), errors.New("exit status 20")
			}
		}
		t.Errorf("the store ran %v, want a show of one reviewed bead", args)
		return nil, nil, errors.New("unexpected store call")
	}
	town := t.TempDir()
	env := Env{Now: func() time.Time { return now }, Town: town}
	env.Tail = func(string, int64) ([]byte, error) { return []byte(content), nil }
	env.Beads = func(ctx context.Context, dir string) BeadStore {
		return beads.New(dir, beads.WithCallContext(ctx), beads.WithRunner(answer))
	}
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          2,
		"om_rejections_1h":       0,
		"om_mean_score_1h":       8.0,
		"om_second_attempts_1h":  0,
		"om_polecat_reviews_1h":  1,
		"om_crew_reviews_1h":     0,
		"om_overseer_reviews_1h": 0,
		"om_unknown_reviews_1h":  1,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v", cols, want)
	}
	// The unknown author is the answer to a read the store did make, not a bead
	// the source never asked about.
	if want := []string{"gt-a1", "gt-gone"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("the store was asked for %v, want %v", asked, want)
	}
}

// The store's read of one reviewed bead, recorded through the real seam over a
// fake bd: the seam runs `bd --allow-stale show --json <id>` with its working
// directory set to the directory the store was opened in, where the pre-seam
// reader ran `bd -C <dir> show --json -- <id>`. The argv therefore differs three
// ways: the seam adds --allow-stale, -C <dir> becomes the directory the store is
// opened in, and the `--` is gone because isBeadID refuses a dash id before the
// store is asked. A read may now be as stale as the store last synced, which the
// hourly counts tolerate; the overseer accepted that as this bead's one
// behaviour change (gt-vu312).
func TestReviewBeadsRunsTheStoreSeamsArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rig     string
		wantDir func(town string) string
	}{
		{
			name:    "a rig's review reads the store in the rig's directory",
			rig:     "gastown",
			wantDir: func(town string) string { return filepath.Join(town, "gastown") },
		},
		{
			name:    "a review with no rig reads the town's store",
			rig:     "",
			wantDir: func(town string) string { return town },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var storeDirs []string
			var reads [][]string
			answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
				// The seam's probe that bd takes --allow-stale, keyed the way
				// internal/beads' own tests key it.
				if len(args) >= 2 && args[0] == "--allow-stale" && args[1] == "version" {
					return []byte("bd version 1.2.2\n"), nil, nil
				}
				reads = append(reads, append([]string(nil), args...))
				return []byte(`[{"id":"gt-s1","notes":"READY TO LAND\nBranch: crew/merlin/gt-s1\nHead: abc\n"}]`), nil, nil
			}
			town := t.TempDir()
			env := Env{Town: town, Beads: func(ctx context.Context, storeDir string) BeadStore {
				storeDirs = append(storeDirs, storeDir)
				return beads.New(storeDir, beads.WithCallContext(ctx), beads.WithRunner(answer))
			}}
			rows, ok := reviewBeads(context.Background(), env, []Review{{Bead: "gt-s1", Rig: tc.rig}}, reviewBdBudget)
			if !ok {
				t.Fatalf("reviewBeads failed, want the bead read through the seam")
			}
			want := []string{"--allow-stale", "show", "--json", "gt-s1"}
			if len(reads) != 1 || !reflect.DeepEqual(reads[0], want) {
				t.Fatalf("the store ran %v, want one %v", reads, want)
			}
			if wantDir := tc.wantDir(town); len(storeDirs) != 1 || storeDirs[0] != wantDir {
				t.Errorf("the store was opened in %v, want %q: -C <dir> is the store's own directory now",
					storeDirs, wantDir)
			}
			if len(rows) != 1 || rows[0].Branch != "crew/merlin/gt-s1" {
				t.Errorf("rows = %+v, want the bead's branch read through the seam", rows)
			}
		})
	}
}

// A snapshot with no store seam cannot attribute, so its kind columns are null:
// a review whose branch was never read is not a review by an unknown author.
func TestSourceReviewWithoutAStoreLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-30*time.Minute)) + "\n"
	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, content, false, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 1 {
		t.Errorf("om_reviews_1h = %v, want the review it could count", cols["om_reviews_1h"])
	}
	for _, column := range reviewKindColumnNames {
		if value, ok := cols[column]; !ok || value != nil {
			t.Errorf("%s = %v, want null without a store", column, value)
		}
	}
}

func TestSourceReviewMissingLogIsAnHourWithNoReviews(t *testing.T) {
	t.Parallel()
	store := &reviewStoreFake{}
	env := reviewTestEnv(t, "", true, nil)
	env.Beads = store.seam()
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          0,
		"om_rejections_1h":       0,
		"om_mean_score_1h":       nil,
		"om_second_attempts_1h":  0,
		"om_polecat_reviews_1h":  0,
		"om_crew_reviews_1h":     0,
		"om_overseer_reviews_1h": 0,
		"om_unknown_reviews_1h":  0,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v (no review is not a score of zero)", cols, want)
	}
	if len(store.reads) != 0 {
		t.Errorf("store reads = %v, want none: an hour of no reviews has no bead to read", store.reads)
	}
}

func TestSourceReviewSkipsEveryLineItCannotUse(t *testing.T) {
	t.Parallel()
	content := "{\n" + `{"bead":"","verdict":"approve","at":"2026-10-24T12:30:00Z"}` + "\n" + `{"bead":"gt-a1"}` + "\n"
	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, content, false, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 0 || cols["om_mean_score_1h"] != nil {
		t.Fatalf("review = %v; want no reviews and no mean from lines it cannot attribute", cols)
	}
	// No review is every kind at zero, not an attribution nobody could make:
	// there is no bead to read and no store is needed.
	for _, column := range reviewKindColumnNames {
		if value, ok := cols[column]; !ok || value != 0 {
			t.Errorf("%s = %v, want 0: an hour with no reviews", column, value)
		}
	}
}

func TestSourceReviewAnUnreadableLogLeavesTheGroupNull(t *testing.T) {
	t.Parallel()
	env := reviewTestEnv(t, "", false, fs.ErrPermission)
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err == nil || cols != nil {
		t.Fatalf("Collect = %v, %v; want an error and no columns", cols, err)
	}
	rec := Collect(context.Background(), env, []Source{reviewSource{}}, SourceTimeout)
	if rec.Groups["review"] != nil || len(rec.Errors) != 1 || rec.Errors[0].Group != "review" {
		t.Errorf("record review = %v, errors = %v; want null and one review error", rec.Groups["review"], rec.Errors)
	}
}
