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

// evalRedHold returns the sweep reasons that hold dispatch: one per kind, sorted
// by kind, each keeping prev's Since when the kind already held and taking now
// when it is new. A sweep under redHoldSweepCycles contributes nothing; one at or
// above it is sweep-no-result when the result was unread and sweep-red otherwise,
// with Detail naming every rig the kind covers.
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
