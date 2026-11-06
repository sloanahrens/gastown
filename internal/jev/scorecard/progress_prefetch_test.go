package scorecard

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// countingProgressNotes reads a progress bead's notes one at a time through
// Notes and in batches through ShowAll, and records both. omit names the beads a
// batch leaves out, fail names the beads whose one-at-a-time read fails, and
// batchErr fails every batch.
type countingProgressNotes struct {
	notes    map[string]string
	omit     map[string]bool
	fail     map[string]error
	batchErr error
	reads    []string
	batches  [][]string
}

func (c *countingProgressNotes) Notes(_, bead string) (string, error) {
	c.reads = append(c.reads, bead)
	if err := c.fail[bead]; err != nil {
		return "", err
	}
	n, ok := c.notes[bead]
	if !ok {
		return "", beads.ErrNotFound
	}
	return n, nil
}

func (c *countingProgressNotes) ShowAll(_ string, ids []string) (map[string]*beads.Issue, error) {
	c.batches = append(c.batches, append([]string(nil), ids...))
	if c.batchErr != nil {
		return nil, c.batchErr
	}
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if n, ok := c.notes[id]; ok && !c.omit[id] {
			out[id] = &beads.Issue{ID: id, Notes: n}
		}
	}
	return out, nil
}

// parseProgressNudges parses each case's progress line through a, as Read does,
// and returns the items in case order.
func parseProgressNudges(t *testing.T, a *progressAdapter, cases []progressCase) []Item {
	t.Helper()
	items := make([]Item, 0, len(cases))
	for _, c := range cases {
		it, err := a.Parse(mustMarshal(t, progressLine(c)))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	return items
}

// progressLabels parses cases into a, prefetches them when prefetch is set, and
// returns each item's labeling in case order.
func progressLabels(t *testing.T, a *progressAdapter, cases []progressCase, prefetch bool) []Labeling {
	t.Helper()
	items := parseProgressNudges(t, a, cases)
	if prefetch {
		a.Prefetch(context.Background(), items)
	}
	out := make([]Labeling, len(items))
	for i, it := range items {
		lab, err := a.Label(context.Background(), it)
		if err != nil {
			t.Fatalf("Label(%s): %v", it.ID, err)
		}
		out[i] = lab
	}
	return out
}

// Two hundred nudges on twenty beads of one rig are labeled from one batch of
// the rig, with no bead read one at a time, and the labels are the ones the
// one-at-a-time path gives.
func TestProgressPrefetchReadsNotesInOneBatchPerRig(t *testing.T) {
	t.Parallel()
	now := progressBase.Add(6 * time.Hour)
	// Every bead is in the store, so the batch returns each one.
	notes := map[string]string{}
	for i := range 20 {
		bead := fmt.Sprintf("gt-pb%02d", i)
		notes[bead] = ""
		if i%2 == 0 {
			notes[bead] = land.LandingRecord{LandedCommit: "abc123", LandedAt: progressBase.Add(time.Duration(i)*time.Minute + 5*time.Minute)}.NoteBlock()
		}
	}
	var cases []progressCase
	for i := range 200 {
		cases = append(cases, progressCase{
			bead:  fmt.Sprintf("gt-pb%02d", i%20),
			nudge: progressBase.Add(time.Duration(i) * time.Minute),
		})
	}
	town := t.TempDir()

	oneByOne := &countingProgressNotes{notes: notes}
	want := progressLabels(t, newProgressTestAdapter(town, oneByOne, now), cases, false)

	batched := &countingProgressNotes{notes: notes}
	got := progressLabels(t, newProgressTestAdapter(town, batched, now), cases, true)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("batched labels differ from the one-at-a-time labels\n got: %v\nwant: %v", got, want)
	}
	labeled := 0
	for _, lab := range got {
		if lab.Labeled {
			labeled++
		}
	}
	if labeled == 0 {
		t.Fatal("no nudge was labeled, so the comparison proves nothing")
	}
	if len(batched.batches) > 3 {
		t.Errorf("batch reads = %d for one rig, want at most 3", len(batched.batches))
	}
	if len(batched.reads) != 0 {
		t.Errorf("notes read one at a time: %v, want none", batched.reads)
	}
}

// A bead the batch omits is read one at a time and labeled as the one-at-a-time
// path labels it, and a bead the store does not hold stays unlabeled. The batch
// succeeded, so nothing is warned.
func TestProgressPrefetchOmittedBeadFallsBackPerBead(t *testing.T) {
	t.Parallel()
	landed := land.LandingRecord{LandedCommit: "abc123", LandedAt: progressBase.Add(10 * time.Minute)}.NoteBlock()
	notes := map[string]string{"gt-fb1": landed, "gt-fb2": landed}
	cases := []progressCase{
		{bead: "gt-fb1", nudge: progressBase},
		{bead: "gt-fb2", nudge: progressBase},
		{bead: "gt-fb3", nudge: progressBase, notInStore: true},
	}
	town := t.TempDir()
	want := progressLabels(t, newProgressTestAdapter(town, &countingProgressNotes{notes: notes}, progressNow), cases, false)

	batched := &countingProgressNotes{notes: notes, omit: map[string]bool{"gt-fb1": true}}
	a := newProgressTestAdapter(town, batched, progressNow)
	var warnings []string
	a.warn = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	got := progressLabels(t, a, cases, true)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels with an omitted bead differ from the one-at-a-time labels\n got: %v\nwant: %v", got, want)
	}
	if !got[0].Labeled {
		t.Fatal("the omitted bead is unlabeled, so the fallback read proves nothing")
	}
	if wantReads := []string{"gt-fb1", "gt-fb3"}; !reflect.DeepEqual(batched.reads, wantReads) {
		t.Errorf("one-at-a-time reads = %v, want %v: the omitted bead and the missing one", batched.reads, wantReads)
	}
	if len(warnings) != 0 {
		t.Errorf("a batch that succeeded warned %q, want nothing", warnings)
	}
}

// A failed batch changes how the notes are read, not the labels: every bead is
// read one at a time, and the failure is named on stderr.
func TestProgressPrefetchFailedBatchFallsBackAndWarns(t *testing.T) {
	t.Parallel()
	landed := land.LandingRecord{LandedCommit: "abc123", LandedAt: progressBase.Add(10 * time.Minute)}.NoteBlock()
	notes := map[string]string{"gt-fb1": landed, "gt-fb2": ""}
	cases := []progressCase{
		{bead: "gt-fb1", nudge: progressBase},
		{bead: "gt-fb2", nudge: progressBase},
		{bead: "gt-fb3", nudge: progressBase, notInStore: true},
	}
	town := t.TempDir()
	want := progressLabels(t, newProgressTestAdapter(town, &countingProgressNotes{notes: notes}, progressNow), cases, false)

	failing := &countingProgressNotes{notes: notes, batchErr: errors.New("bd down")}
	a := newProgressTestAdapter(town, failing, progressNow)
	var warnings []string
	a.warn = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	got := progressLabels(t, a, cases, true)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels after a failed batch differ from the one-at-a-time labels\n got: %v\nwant: %v", got, want)
	}
	if wantReads := []string{"gt-fb1", "gt-fb2", "gt-fb3"}; !reflect.DeepEqual(failing.reads, wantReads) {
		t.Errorf("one-at-a-time reads = %v, want %v", failing.reads, wantReads)
	}
	wantWarnings := []string{"reading gastown's progress notes in a batch: bd down"}
	if !reflect.DeepEqual(warnings, wantWarnings) {
		t.Errorf("warnings = %q, want %q: the failed batch named, once", warnings, wantWarnings)
	}
}

// A bead the batch omits is still read one at a time, and a failure of that
// read fails the label as it does without Prefetch.
func TestProgressPrefetchOmittedBeadReadErrorFailsLabel(t *testing.T) {
	t.Parallel()
	cases := []progressCase{{bead: "gt-fb1", nudge: progressBase}}
	town := t.TempDir()
	reads := &countingProgressNotes{
		notes: map[string]string{"gt-fb1": ""},
		omit:  map[string]bool{"gt-fb1": true},
		fail:  map[string]error{"gt-fb1": errors.New("bead store unavailable")},
	}
	a := newProgressTestAdapter(town, reads, progressNow)
	items := parseProgressNudges(t, a, cases)
	a.Prefetch(context.Background(), items)

	if _, err := a.Label(context.Background(), items[0]); err == nil {
		t.Error("Label returned no error for a failed one-at-a-time read of an omitted bead")
	}
}

// A nudge younger than the settle window is unlabeled by Label, so Prefetch
// reads no bead for it: the batch is never asked for it.
func TestProgressPrefetchSkipsNudgesInsideTheSettleWindow(t *testing.T) {
	t.Parallel()
	cases := []progressCase{{bead: "gt-fresh", nudge: progressBase}}
	// The line is logged a minute after its nudge, so at this time it is one
	// minute short of the settle window.
	now := progressBase.Add(progressSettle - time.Minute)
	reads := &countingProgressNotes{notes: map[string]string{"gt-fresh": ""}}
	a := newProgressTestAdapter(t.TempDir(), reads, now)
	items := parseProgressNudges(t, a, cases)

	a.Prefetch(context.Background(), items)

	if len(reads.batches) != 0 {
		t.Errorf("batch reads = %v, want none for a nudge inside the settle window", reads.batches)
	}
	if lab, err := a.Label(context.Background(), items[0]); err != nil || lab.Labeled {
		t.Errorf("Label = %+v, %v; want unlabeled and no error", lab, err)
	}
}
