package landworker

import (
	"context"
	"errors"
	"strings"
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

// A waiting submission is recorded before the worker lands it, so the record
// exists before the landing removes the label and the branch.
func TestPassRecordsASubmissionBeforeLandingIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	fs := &fakeFirstSight{lander: h.lander}
	h.w.FirstSight = fs

	h.w.Pass(context.Background())

	if len(fs.seen) != 1 || fs.seen[0].BeadID != "gt-a" || fs.seen[0].Head != noteHead || fs.seen[0].Rig != "gastown" {
		t.Fatalf("recorded %+v, want gt-a at its ready head in the rig", fs.seen)
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
	h.w.FirstSight = &fakeFirstSight{lander: h.lander, err: errors.New("disk full")}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, format) }

	h.w.Pass(context.Background())

	if len(h.lander.calls) != 2 {
		t.Fatalf("landed %d of 2 submissions with the record failing", len(h.lander.calls))
	}
	failures := 0
	for _, l := range logs {
		if strings.Contains(l, "recording the submission") {
			failures++
		}
	}
	if failures != 1 {
		t.Errorf("logged the record failure %d times, want once for the run", failures)
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
