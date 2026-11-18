package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// aheadHoldSpecs is n clean beads gt-00..gt-(n-1), oldest first, so the board
// order and the age order are the same.
func aheadHoldSpecs(n int) []specdispatch.Spec {
	var specs []specdispatch.Spec
	for i := 0; i < n; i++ {
		specs = append(specs, cleanSpec(fmt.Sprintf("gt-%02d", i), 1, fmt.Sprintf("2026-09-29T%02d:00:00Z", i)))
	}
	return specs
}

// aheadHoldEstimates turns the estimate-ahead pass on in env, with the
// production estimate writing into db, and records each bead the pass reads
// through the fake's own read.
func aheadHoldEstimates(env *specDispatchEnv, db *beadsfake.Fake, reads *[]string) {
	env.StoryPointShadow = true
	read := env.EstimateAheadRead
	env.EstimateAheadRead = func(ctx context.Context, id string) (*beads.Issue, []beads.Comment, error) {
		*reads = append(*reads, id)
		return read(ctx, id)
	}
	env.StoryPointEstimate = specStoryPointEstimateFor(
		func(string) specEstimateStore { return db },
		func(string) specEstimateRunner { return specEstimateRunnerFor(specEstimateOK()) })
}

// TestEstimateAheadUnderHoldEstimatesEightAndSlingsNothing: with the operator
// hold file the only hold, the tick slings nothing, reads the first eight shaped
// candidates the pass reads, and estimates the pass's own pick of them.
func TestEstimateAheadUnderHoldEstimatesEightAndSlingsNothing(t *testing.T) {
	t.Parallel()
	specs := aheadHoldSpecs(10)
	f := newFakeSpecTown(specs...)
	f.hold = "operator dispatch hold present (/town/hold); remove it to resume"
	env := f.env()
	env.HoldFileOnly = func() bool { return true }
	db := specAheadSeed(t, specs...)
	var reads []string
	aheadHoldEstimates(&env, db, &reads)

	r := runSpecDispatchCycle(env)

	if r.Hold != f.hold {
		t.Fatalf("report hold %q, want the operator hold %q", r.Hold, f.hold)
	}
	if len(f.slung) != 0 {
		t.Fatalf("slung %v under the hold, want none", f.slung)
	}
	var wantReads []string
	for _, s := range specs[:specEstimateAheadReadLimit] {
		wantReads = append(wantReads, s.ID)
	}
	if !slices.Equal(reads, wantReads) {
		t.Fatalf("read %v, want the first %d shaped candidates %v", reads, specEstimateAheadReadLimit, wantReads)
	}
	var estimated []string
	for _, s := range specs {
		if slices.Contains(specAheadLabelsOn(t, db, s.ID), "sp:5") {
			estimated = append(estimated, s.ID)
		}
	}
	if want := []string{"gt-00", "gt-01"}; !slices.Equal(estimated, want) {
		t.Fatalf("estimated %v, want the pass's pick %v", estimated, want)
	}
	notes := strings.Join(r.Notices, "\n")
	if !strings.Contains(notes, "spec dispatch held ("+f.hold+"): nothing slung") {
		t.Errorf("notices %q carry no line that the tick ran under the hold", r.Notices)
	}
	if !strings.Contains(notes, "reading the first 8 of 10 shaped candidates") {
		t.Errorf("notices %q carry no read-limit line", r.Notices)
	}
}

// TestEstimateAheadUnderHoldOnlyTheOperatorFileQualifies: the pass runs under
// the hold only when the operator hold file is the one hold in force. ESTOP and
// the boot hold each stop the whole tick, pass included, and the tick slings and
// estimates nothing.
func TestEstimateAheadUnderHoldOnlyTheOperatorFileQualifies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		otherHold func(t *testing.T, town string)
		wantOnly  bool
	}{
		{name: "operator file alone", otherHold: func(*testing.T, string) {}, wantOnly: true},
		{name: "town ESTOP", otherHold: func(t *testing.T, town string) {
			if err := os.WriteFile(estop.FilePath(town), estop.Render(estop.TriggerManual, "estimate under hold test", specTestNow), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "boot hold", otherHold: func(t *testing.T, town string) {
			path := dispatch.BootHealthPath(town)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("daemon stalled\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := os.WriteFile(dispatch.HoldFilePath(town), []byte("hold\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.otherHold(t, town)
			if got := specOperatorHoldOnly(town); got != tc.wantOnly {
				t.Fatalf("specOperatorHoldOnly = %v, want %v", got, tc.wantOnly)
			}
			if tc.wantOnly {
				return
			}
			specs := aheadHoldSpecs(3)
			f := newFakeSpecTown(specs...)
			f.hold = "town ESTOP active"
			env := f.env()
			env.HoldFileOnly = func() bool { return specOperatorHoldOnly(town) }
			db := specAheadSeed(t, specs...)
			var reads []string
			aheadHoldEstimates(&env, db, &reads)

			r := runSpecDispatchCycle(env)

			if r.Hold == "" {
				t.Fatalf("tick under the %s reports no hold", tc.name)
			}
			if len(f.slung) != 0 {
				t.Fatalf("tick under the %s slung %v, want none", tc.name, f.slung)
			}
			if len(reads) != 0 {
				t.Fatalf("tick under the %s read %v for the pass, want none", tc.name, reads)
			}
			if n := aheadNotices(r.Notices); len(n) != 0 {
				t.Fatalf("tick under the %s wrote estimate-ahead lines %q, want none", tc.name, n)
			}
		})
	}
}

// TestEstimateAheadUnderHoldNoHoldIsUnchanged: a tick with no hold never asks
// the hold-only question and slings as it always did.
func TestEstimateAheadUnderHoldNoHoldIsUnchanged(t *testing.T) {
	t.Parallel()
	specs := aheadHoldSpecs(2)
	f := newFakeSpecTown(specs...)
	env := f.env()
	env.PerTick = 2
	asked := 0
	env.HoldFileOnly = func() bool {
		asked++
		return true
	}

	r := runSpecDispatchCycle(env)

	if asked != 0 {
		t.Fatalf("hold-only question asked %d times on a tick with no hold, want none", asked)
	}
	if r.Hold != "" {
		t.Fatalf("report hold %q with no hold in force", r.Hold)
	}
	if got := slices.Sorted(slices.Values(f.slung)); !slices.Equal(got, []string{"gt-00", "gt-01"}) {
		t.Fatalf("slung %v, want both beads", got)
	}
	for _, n := range r.Notices {
		if strings.Contains(n, "spec dispatch held") {
			t.Fatalf("notice %q on a tick with no hold", n)
		}
	}
}

// TestEstimateAheadUnderHoldBoardTimeoutEstimatesNothing: a held board read
// that has not returned when the read phase's timeout passes ends the pass in
// one line, and nothing is estimated.
func TestEstimateAheadUnderHoldBoardTimeoutEstimatesNothing(t *testing.T) {
	t.Parallel()
	specs := aheadHoldSpecs(2)
	f := newFakeSpecTown(specs...)
	f.hold = "operator dispatch hold present"
	env := f.env()
	env.HoldFileOnly = func() bool { return true }
	env.StoryPointShadow = true
	block := make(chan struct{})
	defer close(block)
	env.HoldCandidates = func() specBoardRead {
		<-block
		return specBoardRead{}
	}
	fired := make(chan time.Time)
	close(fired)
	env.EstimateAheadAfter = func(time.Duration) <-chan time.Time { return fired }
	estimated := 0
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		estimated++
		return specEstimateOutcome{}, nil
	}

	r := runSpecDispatchCycle(env)

	if estimated != 0 {
		t.Fatalf("estimated %d beads after the held read timed out, want none", estimated)
	}
	if len(f.slung) != 0 {
		t.Fatalf("slung %v after the held read timed out, want none", f.slung)
	}
	if n := aheadNotices(r.Notices); len(n) != 1 || !strings.Contains(n[0], "did not finish within") {
		t.Fatalf("estimate-ahead lines %q, want one timeout line", n)
	}
}

// TestEstimateAheadUnderHoldLeavesHeldAndUnreadBeadsOut: a candidate in a rig
// on hold, and one whose blockers cannot be read, are not estimated; the rest
// of the board is.
func TestEstimateAheadUnderHoldLeavesHeldAndUnreadBeadsOut(t *testing.T) {
	t.Parallel()
	held := cleanSpec("gt-held", 1, "2026-09-29T00:00:00Z")
	unread := cleanSpec("gt-unread", 1, "2026-09-29T01:00:00Z")
	unread.Description += "\n\n## Files\nmodify: x.go\n"
	open := cleanSpec("gt-open", 1, "2026-09-29T02:00:00Z")
	specs := []specdispatch.Spec{held, unread, open}
	f := newFakeSpecTown(specs...)
	f.rigOf = map[string]string{held.ID: "otherrig"}
	f.rigHold["otherrig"] = "rig ESTOP active"
	f.hold = "operator dispatch hold present"
	env := f.env()
	env.HoldFileOnly = func() bool { return true }
	env.BlockerRead = func(string) (specdispatch.Spec, []string, error) {
		return specdispatch.Spec{}, nil, errors.New("blocker graph unreadable")
	}
	db := specAheadSeed(t, specs...)
	var reads []string
	aheadHoldEstimates(&env, db, &reads)

	r := runSpecDispatchCycle(env)

	if want := []string{"gt-open"}; !slices.Equal(reads, want) {
		t.Fatalf("read %v for the pass, want only %v", reads, want)
	}
	if n := strings.Join(r.Notices, "\n"); !strings.Contains(n, "gt-unread: story points estimate ahead: cannot read its blockers") {
		t.Fatalf("notices %q carry no blocker-read line for gt-unread", r.Notices)
	}
	if slices.Contains(specAheadLabelsOn(t, db, held.ID), "sp:5") {
		t.Fatalf("held-rig bead %s was estimated under the hold", held.ID)
	}
}
