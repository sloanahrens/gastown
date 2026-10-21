package landworker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// fakeFirstSight records each submission it is given and how many landings had
// started when it was given it, and fails with err when set.
type fakeFirstSight struct {
	seen     []land.Work
	landedAt []int
	err      error
	lander   *fakeLander
}

func (f *fakeFirstSight) Record(w land.Work) (bool, error) {
	f.seen = append(f.seen, w)
	f.landedAt = append(f.landedAt, len(f.lander.calls))
	if f.err != nil {
		return false, f.err
	}
	return true, nil
}

// captureLogs renders the worker's log lines into the returned slice, so a
// test reads the messages the worker writes rather than their format strings.
func captureLogs(w *Worker) *[]string {
	logs := &[]string{}
	w.Logf = func(format string, args ...any) { *logs = append(*logs, fmt.Sprintf(format, args...)) }
	return logs
}

// recordFailureLine is the whole log line for a failed first-sight record of
// id, so a test counts the lines about its beads rather than any line whose
// format happens to mention the phrase.
func recordFailureLine(id string, err error) string {
	return fmt.Sprintf("landing_worker: gastown: %s: recording the submission for the jev battery: %v", id, err)
}

// errDiskFull is the error a first-sight record fails with when the store
// cannot be written.
var errDiskFull = errors.New("disk full")

// countRecordFailures counts the log lines that report a failed first-sight
// record for one of ids, matching the whole rendered line.
func countRecordFailures(logs, ids []string, err error) int {
	n := 0
	for _, l := range logs {
		for _, id := range ids {
			if l == recordFailureLine(id, err) {
				n++
			}
		}
	}
	return n
}

// A waiting submission is recorded before the worker lands it, so the record
// exists before the landing removes the label and the branch.
func TestPassRecordsASubmissionBeforeLandingIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	fs := &fakeFirstSight{lander: h.lander}
	h.w.FirstSight = fs

	h.w.Pass(context.Background())

	// Two records and no more: the ready head, then the tip the landing takes.
	wantHeads := []string{noteHead, tipHead}
	if len(fs.seen) != len(wantHeads) {
		t.Fatalf("recorded %+v, want gt-a at its ready head then at the tip it lands", fs.seen)
	}
	for i, head := range wantHeads {
		if w := fs.seen[i]; w.BeadID != "gt-a" || w.Head != head || w.Rig != "gastown" {
			t.Fatalf("recorded %+v at %d, want gt-a at %s in the rig", fs.seen[i], i, head)
		}
	}
	if fs.landedAt[0] != 0 {
		t.Errorf("recorded after %d landings started, want before any", fs.landedAt[0])
	}
	if len(h.lander.calls) != 1 {
		t.Errorf("landed %d times, want the submission landed", len(h.lander.calls))
	}
}

// A record that cannot be written is logged once for the run of failures, and
// the landings go on.
func TestPassFirstSightFailureLogsOnceAndLandsAnyway(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	h.w.FirstSight = &fakeFirstSight{lander: h.lander, err: errDiskFull}
	logs := captureLogs(h.w)

	h.w.Pass(context.Background())

	if len(h.lander.calls) != 2 {
		t.Fatalf("landed %d of 2 submissions with the record failing", len(h.lander.calls))
	}
	failures := countRecordFailures(*logs, []string{"gt-a", "gt-b"}, errDiskFull)
	if failures != 1 {
		t.Errorf("logged the record failure %d times for gt-a and gt-b, want once for the run", failures)
	}
}

// A bead with no READY TO LAND block has no submission to record: it is left
// to process, which reports it, and the recorder is not asked.
func TestPassFirstSightSkipsABeadWithNoReadyBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.bd.Seed(beads.Issue{ID: "gt-bare", Title: "work", Status: "hooked", Type: "task",
		Labels: []string{land.LabelReadyToLand}})
	fs := &fakeFirstSight{lander: h.lander}
	h.w.FirstSight = fs

	h.w.Pass(context.Background())

	if len(fs.seen) != 0 {
		t.Errorf("recorded a bead with no READY TO LAND block: %+v", fs.seen)
	}
}

// keyedFirstSight fails the records of the beads in failing, and writes
// nothing for the beads already on file, as the recorder does for a dedupe hit.
type keyedFirstSight struct {
	seen    []land.Work
	onFile  map[string]bool
	failing map[string]bool
}

func (f *keyedFirstSight) Record(w land.Work) (bool, error) {
	f.seen = append(f.seen, w)
	if f.failing[w.BeadID] {
		return false, errors.New("disk full")
	}
	return !f.onFile[w.BeadID], nil
}

// The branch moved past the READY head, so the landing takes the tip: the tip
// gets its own first-sight record, written before the landing, so the battery
// asks about the commits the landing adds.
func TestPassRecordsTheTipWhenTheBranchMovedPastTheReadyHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	fs := &fakeFirstSight{lander: h.lander}
	h.w.FirstSight = fs

	h.w.Pass(context.Background())

	tipAt := -1
	for i, w := range fs.seen {
		if w.Head == tipHead {
			tipAt = i
		}
	}
	if tipAt < 0 {
		t.Fatalf("recorded %+v, want a record at the landed tip %s", fs.seen, tipHead)
	}
	if fs.landedAt[tipAt] != 0 {
		t.Errorf("tip recorded after %d landings started, want before any", fs.landedAt[tipAt])
	}
	if len(h.lander.calls) != 1 || h.lander.calls[0].Head != tipHead {
		t.Fatalf("landed %+v, want the tip %s", h.lander.calls, tipHead)
	}
}

// A record already on file writes nothing, so it does not end a run of failed
// writes: the failure is logged once across passes, not once per pass.
func TestPassFirstSightFailureRunSpansRecordsOnFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		order   []string // the landing order, first to last
		failing []string
		onFile  []string
	}{
		{name: "a record on file after the failing bead", order: []string{"gt-a", "gt-b"}, failing: []string{"gt-a"}, onFile: []string{"gt-b"}},
		{name: "a record on file between two failing beads", order: []string{"gt-a", "gt-b", "gt-c"}, failing: []string{"gt-a", "gt-c"}, onFile: []string{"gt-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			for i, id := range tc.order {
				h.seedReadyAt(t, id, 2, fmt.Sprintf("2026-09-30T1%d:00:00Z", i))
			}
			h.lander.fn = func(int, land.Work) (land.Result, error) {
				return land.Result{}, fmt.Errorf("%w: waiting on the gate", land.ErrNotReady)
			}
			fs := &keyedFirstSight{failing: map[string]bool{}, onFile: map[string]bool{}}
			for _, id := range tc.failing {
				fs.failing[id] = true
			}
			for _, id := range tc.onFile {
				fs.onFile[id] = true
			}
			h.w.FirstSight = fs
			logs := captureLogs(h.w)

			h.w.Pass(context.Background())
			h.w.Pass(context.Background())

			failures := countRecordFailures(*logs, tc.failing, errDiskFull)
			if failures != 1 {
				t.Errorf("logged the record failure %d times across two passes, want once for the run", failures)
			}
		})
	}
}
