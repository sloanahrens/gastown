package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

// batchBeads serves beads from a map and counts the calls made to it, one at a
// time and in batches. A missing id is beads.ErrNotFound for Show and absent
// from a batch, as the production reader does it.
type batchBeads struct {
	issues  map[string]*beads.Issue
	shows   []string
	batches [][]string
	// batchErr fails every batch, leaving a one-at-a-time read to answer.
	batchErr error
}

func (b *batchBeads) Show(id string) (*beads.Issue, error) {
	b.shows = append(b.shows, id)
	if issue, ok := b.issues[id]; ok {
		return issue, nil
	}
	return nil, beads.ErrNotFound
}

func (b *batchBeads) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	b.batches = append(b.batches, append([]string(nil), ids...))
	if b.batchErr != nil {
		return nil, b.batchErr
	}
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := b.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

// withoutPrefetch hides an adapter's Prefetch, so Build labels it one item at
// a time: the path every adapter took before batching.
type withoutPrefetch struct{ Adapter }

func reportJSON(t *testing.T, a Adapter) string {
	t.Helper()
	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestShowAllBatchesDedupesAndFallsBack(t *testing.T) {
	t.Parallel()
	issues := map[string]*beads.Issue{}
	var ids []string
	for i := range 250 {
		id := fmt.Sprintf("gt-%03d", i)
		issues[id] = &beads.Issue{ID: id}
		ids = append(ids, id, id)
	}
	ids = append(ids, "", "--flag", "gt-missing")
	store := &batchBeads{issues: issues}

	got, err := showAll(store, ids)
	if err != nil {
		t.Errorf("showAll: %v", err)
	}
	if len(got) != 250 {
		t.Errorf("showAll found %d beads, want 250", len(got))
	}
	if len(store.batches) != 3 {
		t.Errorf("showAll made %d batch calls for 251 distinct ids, want 3", len(store.batches))
	}
	for _, batch := range store.batches {
		for _, id := range batch {
			if id == "" || strings.HasPrefix(id, "-") {
				t.Errorf("showAll sent %q to bd", id)
			}
		}
	}
	if len(store.shows) != 0 {
		t.Errorf("showAll read %d beads one at a time", len(store.shows))
	}

	if got, err := showAll(struct{}{}, ids); got != nil || err != nil {
		t.Errorf("a reader that cannot batch gave %d beads and error %v, want none", len(got), err)
	}
	failing := &batchBeads{issues: issues, batchErr: errors.New("bd down")}
	if got, err := showAll(failing, ids); len(got) != 0 || err == nil || !strings.Contains(err.Error(), "bd down") {
		t.Errorf("a failed batch gave %d beads and error %v, want none and the batch error", len(got), err)
	}
}

// countingDiffs sizes commits from a map and counts what it was asked. A
// prefetch stands for the one git pass that sizes every commit it is given.
type countingDiffs struct {
	lines      map[string]int
	prefetches [][]string
	reads      []string
	prefetched map[string]bool
}

func (c *countingDiffs) PrefetchLines(_ context.Context, commits []string) {
	c.prefetches = append(c.prefetches, commits)
	c.prefetched = map[string]bool{}
	for _, commit := range commits {
		c.prefetched[commit] = true
	}
}

func (c *countingDiffs) ChangedLines(_ context.Context, commit string) (int, error) {
	if !c.prefetched[commit] {
		// Not sized by the prefetch: this is the git call per item.
		c.reads = append(c.reads, commit)
	}
	if n, ok := c.lines[commit]; ok {
		return n, nil
	}
	return 0, ErrNoCommit
}

func writeFindingLog(t *testing.T, town string, ids []string) {
	t.Helper()
	var lines []string
	for i, id := range ids {
		line := dedupe.Line{
			TS: time.Date(2026, 10, 18, 9, i, 0, 0, time.UTC), Finding: id, CandidatesOffered: []string{"gt-other"},
			Outcome: dedupe.OutcomeAnswered, Model: "jev-1",
			Answers: map[string]dedupe.Answer{
				dedupe.QFixWorthy: {Noul: fptr(0.8)},
				dedupe.QSeverity:  {Score: fptr(2), Confidence: fptr(0.7)},
			},
		}
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	writeFile(t, dedupe.LogPath(town), strings.Join(lines, "\n")+"\n")
}

// The finding adapter reads every bead in one batch and sizes every landed
// commit in one pass, and its report is the report the one-at-a-time path makes.
func TestFindingPrefetchMatchesPerItemReport(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	issues := map[string]*beads.Issue{}
	diffs := map[string]int{}
	var ids []string
	for i := range 40 {
		id := fmt.Sprintf("gt-f%02d", i)
		ids = append(ids, id)
		sha := fmt.Sprintf("%07x", 0xabc0000+i)
		switch i % 4 {
		case 0:
			issues[id] = closedBead(id, landedReason(sha))
			diffs[sha] = []int{10, 100, 400, 50}[i/4%4]
		case 1:
			issues[id] = closedBead(id, "no-changes: can't reproduce; see gt-other")
		case 2:
			issues[id] = &beads.Issue{ID: id, Status: "open"}
		}
		// i%4 == 3: the bead does not exist.
	}
	// A landed commit the repo does not hold stays unlabeled for severity.
	issues["gt-f00"] = closedBead("gt-f00", landedReason("deadbee"))
	writeFindingLog(t, town, ids)

	oneByOne := &batchBeads{issues: issues}
	want := reportJSON(t, withoutPrefetch{NewFinding(town, oneByOne, &countingDiffs{lines: diffs})})

	store := &batchBeads{issues: issues}
	sizer := &countingDiffs{lines: diffs}
	got := reportJSON(t, NewFinding(town, store, sizer))

	if got != want {
		t.Errorf("the batched report differs from the per-item report\n got: %s\nwant: %s", got, want)
	}
	if len(store.batches) != 1 {
		t.Errorf("finding made %d bead batches for 40 items, want 1", len(store.batches))
	}
	// Only the ten beads the batch did not return are read alone: the ones that
	// do not exist.
	if len(store.shows) != 10 {
		t.Errorf("finding read %d beads one at a time, want the 10 absent ones", len(store.shows))
	}
	if len(sizer.prefetches) != 1 {
		t.Errorf("finding made %d commit passes, want 1", len(sizer.prefetches))
	}
	if len(sizer.reads) != 0 {
		t.Errorf("finding sized %v one commit at a time, want none", sizer.reads)
	}
	if len(sizer.prefetches) == 1 && len(sizer.prefetches[0]) != 10 {
		t.Errorf("the commit pass was given %d commits, want the 10 landed ones", len(sizer.prefetches[0]))
	}
}

// A bead batch that fails changes the time the report takes, not its content.
func TestFindingPrefetchFailureFallsBackToPerItemReads(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	issues := map[string]*beads.Issue{
		"gt-f1": closedBead("gt-f1", "no-changes: nothing to do"),
		"gt-f2": closedBead("gt-f2", landedReason("abc1234")),
	}
	writeFindingLog(t, town, []string{"gt-f1", "gt-f2"})
	diffs := &countingDiffs{lines: map[string]int{"abc1234": 400}}

	want := reportJSON(t, withoutPrefetch{NewFinding(town, &batchBeads{issues: issues}, diffs)})
	failing := &batchBeads{issues: issues, batchErr: errors.New("bd down")}
	if got := reportJSON(t, NewFinding(town, failing, diffs)); got != want {
		t.Errorf("a failed batch changed the report\n got: %s\nwant: %s", got, want)
	}
	if len(failing.shows) != 2 {
		t.Errorf("one-at-a-time reads = %d, want 2", len(failing.shows))
	}
}

func TestDispatchPrefetchReadsEachBeadOnce(t *testing.T) {
	t.Parallel()
	issues := map[string]*beads.Issue{
		"gt-d1": {ID: "gt-d1", Status: "closed", CloseReason: "Landed on main\nlanded_commit: abc123"},
		"gt-d2": {ID: "gt-d2", Status: "open"},
	}
	store := &batchBeads{issues: issues}
	a := dispatchAdapterFor(t.TempDir(), store)
	// gt-d1 is placed twice; gt-d3 does not exist.
	a.Prefetch(context.Background(), []Item{{ID: "gt-d1"}, {ID: "gt-d2"}, {ID: "gt-d1"}, {ID: "gt-d3"}})

	if len(store.batches) != 1 || len(store.batches[0]) != 3 {
		t.Fatalf("batches = %v, want one batch of the 3 distinct beads", store.batches)
	}
	for _, id := range []string{"gt-d1", "gt-d2", "gt-d1"} {
		issue, err := a.showBead(id)
		if err != nil || issue == nil || issue.ID != id {
			t.Errorf("showBead(%s) = %v, %v", id, issue, err)
		}
	}
	if len(store.shows) != 0 {
		t.Errorf("beads read one at a time: %v, want none", store.shows)
	}
	if issue, err := a.showBead("gt-d3"); err != nil || issue != nil {
		t.Errorf("showBead(missing) = %v, %v; want nil, nil", issue, err)
	}
	if len(store.shows) != 1 {
		t.Errorf("a bead the batch lacked was read %d times, want once", len(store.shows))
	}
}

func TestDispatchPrefetchOpensNothingForAnEmptyLog(t *testing.T) {
	t.Parallel()
	opened := 0
	a := newDispatchAdapter(t.TempDir(), func() (beadReader, error) {
		opened++
		return &batchBeads{}, nil
	})
	a.Prefetch(context.Background(), nil)
	if opened != 0 {
		t.Errorf("an empty log opened the beads store %d times", opened)
	}
}

// batchNotes serves notes through the rig-wide batch read and counts its calls.
type batchNotes struct {
	fakeLandingNotes
	batches int
	issues  map[string]*beads.Issue
}

func (b *batchNotes) ShowAll(_ string, ids []string) (map[string]*beads.Issue, error) {
	b.batches++
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := b.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

func TestLandingPrefetchReadsNotesInOneBatchPerRig(t *testing.T) {
	t.Parallel()
	notes := &batchNotes{issues: map[string]*beads.Issue{}}
	notes.byBead = map[string]string{}
	var items []Item
	for i := range 20 {
		id := fmt.Sprintf("gt-l%02d", i)
		items = append(items, landingItem(id, "a1a1a1a"))
		text := land.OverseerReviewMarker + " a1a1a1a PASS"
		notes.issues[id] = &beads.Issue{ID: id, Notes: text}
		notes.byBead[id] = text
	}
	a := &landingAdapter{log: "unused", notes: notes}
	want := make([]Labeling, len(items))
	for i, it := range items {
		lab, err := (&landingAdapter{log: "unused", notes: &notes.fakeLandingNotes}).Label(context.Background(), it)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = lab
	}
	notes.reads = nil

	a.Prefetch(context.Background(), items)
	for i, it := range items {
		lab, err := a.Label(context.Background(), it)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(lab) != fmt.Sprint(want[i]) {
			t.Errorf("item %s: batched labeling %v, per-item labeling %v", it.ID, lab, want[i])
		}
	}
	if notes.batches != 1 {
		t.Errorf("batch reads = %d, want 1 for one rig", notes.batches)
	}
	if len(notes.reads) != 0 {
		t.Errorf("notes read one at a time: %v, want none", notes.reads)
	}
}

func TestSubmissionPrefetchFillsTheNotesCache(t *testing.T) {
	t.Parallel()
	notes := &batchNotes{issues: map[string]*beads.Issue{
		"gt-s1": {ID: "gt-s1", Notes: "n1"},
		"gt-s2": {ID: "gt-s2", Notes: "n2"},
	}}
	notes.byBead = map[string]string{"gt-s3": "n3"}
	a := &submissionAdapter{notes: notes}
	a.Prefetch(context.Background(), []Item{{ID: "gt-s1"}, {ID: "gt-s2"}, {ID: "gt-s1"}, {ID: "gt-s3"}})

	for bead, want := range map[string]string{"gt-s1": "n1", "gt-s2": "n2", "gt-s3": "n3"} {
		got, err := a.notesFor(bead)
		if err != nil || got != want {
			t.Errorf("notesFor(%s) = %q, %v; want %q", bead, got, err, want)
		}
	}
	if notes.batches != 1 {
		t.Errorf("batch reads = %d, want 1", notes.batches)
	}
	if len(notes.reads) != 1 || notes.reads[0] != submission.RigName+"/gt-s3" {
		t.Errorf("one-at-a-time reads = %v, want only the bead the batch lacked", notes.reads)
	}
}

func TestStalePrefetchFillsTheBeadCache(t *testing.T) {
	t.Parallel()
	a := newFakeStaleAdapter(nil)
	notes := &staleBatch{issues: map[string]*beads.Issue{"gt-a": {ID: "gt-a"}, "gt-b": {ID: "gt-b"}}}
	a.issues = notes
	var items []Item
	for _, bead := range []string{"gt-a", "gt-b", "gt-c"} {
		a.lines[bead] = staleLine(bead, 2, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), nil)
		items = append(items, Item{ID: bead})
	}
	a.Prefetch(context.Background(), items)
	for _, bead := range []string{"gt-a", "gt-b"} {
		if issue, err := a.bead("gastown", bead); err != nil || issue == nil {
			t.Errorf("bead(%s) = %v, %v", bead, issue, err)
		}
	}
	if notes.shows != 0 {
		t.Errorf("beads read one at a time: %d, want none", notes.shows)
	}
}

// staleBatch is a stale bead reader that also answers batches.
type staleBatch struct {
	issues map[string]*beads.Issue
	shows  int
}

func (s *staleBatch) Show(_, bead string) (*beads.Issue, error) {
	s.shows++
	if issue, ok := s.issues[bead]; ok {
		return issue, nil
	}
	return nil, beads.ErrNotFound
}

func (s *staleBatch) ShowAll(_ string, ids []string) (map[string]*beads.Issue, error) {
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := s.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

func TestAttentionPrefetchFillsTheBeadCache(t *testing.T) {
	t.Parallel()
	store := &batchBeads{issues: map[string]*beads.Issue{"gt-x": {ID: "gt-x"}, "gt-y": {ID: "gt-y"}}}
	a := newAttentionTestAdapter(t.TempDir(), store)
	items := []Item{{ID: "i1"}, {ID: "i2"}, {ID: "i3"}, {ID: "i4"}}
	for id, bead := range map[string]string{"i1": "gt-x", "i2": "gt-y", "i3": "gt-x", "i4": ""} {
		a.lines[id] = attentionLine(bead)
	}
	a.Prefetch(context.Background(), items)

	for _, bead := range []string{"gt-x", "gt-y", "gt-x"} {
		if issue, err := a.bead(bead); err != nil || issue == nil || issue.ID != bead {
			t.Errorf("bead(%s) = %v, %v", bead, issue, err)
		}
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 2 {
		t.Errorf("batches = %v, want one batch of the 2 distinct beads", store.batches)
	}
	if len(store.shows) != 0 {
		t.Errorf("beads read one at a time: %v, want none", store.shows)
	}
}

func attentionLine(bead string) jevattention.Line { return jevattention.Line{Bead: bead} }

// listingBeads is a store that also answers one list query for many ids.
type listingBeads struct {
	*batchBeads
	lists [][]string
}

func (l *listingBeads) ListByIDs(ids []string) (map[string]*beads.Issue, error) {
	l.lists = append(l.lists, append([]string(nil), ids...))
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := l.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

// A store that can list reads its own beads in one query per chunk, and a show
// batch is left for the ids the query did not return.
func TestShowAllListsFirstAndBatchesTheRest(t *testing.T) {
	t.Parallel()
	issues := map[string]*beads.Issue{}
	var ids []string
	for i := range 250 {
		id := fmt.Sprintf("gt-%03d", i)
		issues[id] = &beads.Issue{ID: id}
		ids = append(ids, id)
	}
	// hq-wisp-1 is held by the store but not listed (a wisp); gt-missing is nowhere.
	store := &listingBeads{batchBeads: &batchBeads{issues: issues}}
	listed := store.batchBeads.issues
	delete(listed, "hq-wisp-1")
	wisp := &beads.Issue{ID: "hq-wisp-1"}
	ids = append(ids, "hq-wisp-1", "gt-missing")
	showOnly := &wispOnlyBatch{listingBeads: store, wisp: wisp}

	got, err := showAll(showOnly, ids)
	if err != nil {
		t.Errorf("showAll: %v", err)
	}
	if len(got) != 251 || got["hq-wisp-1"] == nil {
		t.Errorf("showAll found %d beads, want the 250 listed and the wisp", len(got))
	}
	if len(store.lists) != 3 {
		t.Errorf("list queries = %d for 252 ids, want 3", len(store.lists))
	}
	if len(showOnly.batches) != 1 || len(showOnly.batches[0]) != 2 {
		t.Errorf("show batches = %v, want one batch of the 2 ids the lists missed", showOnly.batches)
	}
	// The one show is the check of the lists against a show.
	if len(store.shows) != 1 {
		t.Errorf("beads read one at a time: %v, want the one check read", store.shows)
	}
}

// wispOnlyBatch adds a wisp to what ShowMultiple can return.
type wispOnlyBatch struct {
	*listingBeads
	wisp    *beads.Issue
	batches [][]string
}

func (w *wispOnlyBatch) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	w.batches = append(w.batches, append([]string(nil), ids...))
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if id == w.wisp.ID {
			out[id] = w.wisp
		}
	}
	return out, nil
}

// A reader that is a beads.Client (what the adapters open in production) is
// listed through beads.ListByIDs, with one query for every id it holds and the
// rest left to the show batch.
func TestShowAllListsAClientThroughListByIDs(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	a, err := fake.Create(beads.CreateOptions{Title: "a", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := fake.Create(beads.CreateOptions{Title: "b", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.CloseWithReason("done", b.ID); err != nil {
		t.Fatal(err)
	}

	got, err := showAll(fake, []string{a.ID, b.ID, "gt-missing"})
	if err != nil {
		t.Errorf("showAll: %v", err)
	}
	if len(got) != 2 || got[a.ID] == nil || got[b.ID] == nil || got[b.ID].Status != "closed" {
		t.Errorf("showAll over a Client = %v, want both held beads, whatever their status", got)
	}
}

// An unset priority reads "0" and is not a set field, so it adds nothing to the
// count that picks the bead bd list is checked against.
func TestLabelFieldsSetLeavesUnsetPriorityOut(t *testing.T) {
	t.Parallel()
	if got := labelFieldsSet(&beads.Issue{Status: "open"}); got != 1 {
		t.Errorf("labelFieldsSet(status only) = %d, want 1", got)
	}
	if got := labelFieldsSet(&beads.Issue{Status: "open", Priority: 2}); got != 2 {
		t.Errorf("labelFieldsSet(status and priority) = %d, want 2", got)
	}
	if got := labelFieldsSet(&beads.Issue{}); got != 0 {
		t.Errorf("labelFieldsSet(empty) = %d, want 0", got)
	}
}
