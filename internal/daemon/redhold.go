package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// redHoldSweepCycles is the consecutive red sweep cycles on one rig that hold dispatch.
const redHoldSweepCycles = 2

// redHoldSweep is one rig's sweep history, and whether its last result was unread.
type redHoldSweep struct {
	Rig       string
	RedCycles int
	NoResult  bool
}

// redHoldInputs is what the sweep triggers decide from.
type redHoldInputs struct{ Sweeps []redHoldSweep }

// redHoldSweepOf converts one rig's sweep record into the evaluator's input; ok
// is false for an unknown record, which the caller leaves out to keep its prior
// state. A RED tier is no-result when the record names tierSweepRunnerNoResult
// as its cause or as its failing name.
func redHoldSweepOf(rig string, st tierSweepState, err error) (redHoldSweep, bool) {
	if err != nil {
		return redHoldSweep{}, false
	}
	s := redHoldSweep{Rig: rig, RedCycles: st.RedCycles}
	for _, tier := range st.Tiers {
		if tier.Verdict != tierSweepRed {
			continue
		}
		if tier.Cause == tierSweepRunnerNoResult || tierSweepNoRunResult(tier) {
			s.NoResult = true
			break
		}
	}
	return s, true
}

// evalRedHold returns the sweep reasons that hold dispatch: one per kind, sorted
// by kind, each keeping prev's Since when the kind already held and taking now when
// new. A sweep under redHoldSweepCycles contributes nothing; at or above it is
// sweep-no-result when unread, sweep-red otherwise, with Detail naming every rig.
func evalRedHold(in redHoldInputs, now time.Time, prev dispatch.RedHoldRecord) dispatch.RedHoldRecord {
	details := map[string][]string{}
	for _, s := range in.Sweeps {
		if s.RedCycles < redHoldSweepCycles {
			continue
		}
		kind := dispatch.RedHoldSweepRed
		if s.NoResult {
			kind = dispatch.RedHoldSweepNoResult
		}
		details[kind] = append(details[kind], fmt.Sprintf("%s: %d red cycles", s.Rig, s.RedCycles))
	}
	var reasons []dispatch.RedHoldReason
	for _, kind := range slices.Sorted(maps.Keys(details)) {
		since := now
		for _, r := range prev.Reasons {
			if r.Kind == kind {
				since = r.Since
				break
			}
		}
		reasons = append(reasons, dispatch.RedHoldReason{Kind: kind, Since: since, Detail: strings.Join(details[kind], ", ")})
	}
	return dispatch.RedHoldRecord{Reasons: reasons}
}

// redHoldChangeLine says what the daemon logs when the red hold changes: "" when
// the kind set is unchanged, 'red hold: set: <sorted kinds>' when next holds a
// different set, and 'red hold: cleared' when none remain. Since and Detail do
// not count, so a reason that merely refreshed them logs nothing.
func redHoldChangeLine(prev, next dispatch.RedHoldRecord) string {
	prevKinds, nextKinds := redHoldKinds(prev), redHoldKinds(next)
	if slices.Equal(prevKinds, nextKinds) {
		return ""
	}
	if len(nextKinds) == 0 {
		return "red hold: cleared"
	}
	return "red hold: set: " + strings.Join(nextKinds, ", ")
}

// redHoldKinds returns the record's distinct kinds, sorted.
func redHoldKinds(rec dispatch.RedHoldRecord) []string {
	seen := make(map[string]bool, len(rec.Reasons))
	for _, r := range rec.Reasons {
		seen[r.Kind] = true
	}
	return slices.Sorted(maps.Keys(seen))
}
