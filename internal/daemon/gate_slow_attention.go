package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// gateSlowWindow is how many of a rig's newest gate runs must all be slow before
// the queue says so, and gateSlowPoleShare is the share of the tier wall the
// longest package may take before a run is pole-bound (gt-023r2e).
const (
	gateSlowWindow    = 3
	gateSlowPoleShare = 0.4
	gateSlowPrefix    = "gate-slow:" // keys every gate-slow item, so a failed read keeps only them
)

// gateSlowPoleBound reports whether run's longest package took more than its
// share of the tier wall. A run with no tier seconds is never pole-bound.
func gateSlowPoleBound(run land.GateRun) bool {
	return run.TierS > 0 && run.PoleS > gateSlowPoleShare*run.TierS
}

// gateSlowItems returns rig's gate-slow item, or none. It holds when the rig's
// last gateSlowWindow runs were all over their tier bar, or all pole-bound. runs
// is in log order, oldest first. The item names the newest run's pole package
// and tier seconds.
func gateSlowItems(runs []land.GateRun, rig string) []attention.Item {
	var mine []land.GateRun
	for _, r := range runs {
		if r.Rig == rig {
			mine = append(mine, r)
		}
	}
	if len(mine) < gateSlowWindow {
		return nil
	}
	last := mine[len(mine)-gateSlowWindow:]
	allOver, allPole := true, true
	for _, r := range last {
		allOver = allOver && r.Over
		allPole = allPole && gateSlowPoleBound(r)
	}
	if !allOver && !allPole {
		return nil
	}
	newest := last[len(last)-1]
	reason := "pole-bound"
	if allOver {
		reason = "over the tier bar"
	}
	pole := newest.PolePkg
	if pole == "" {
		pole = "unknown"
	}
	return []attention.Item{{
		Key:      gateSlowPrefix + rig,
		Severity: attention.SeverityMedium,
		Rig:      rig,
		Summary: fmt.Sprintf("%s: the last %d gate runs were %s; tier %.0fs, pole %s %.0fs",
			rig, gateSlowWindow, reason, newest.TierS, pole, newest.PoleS),
	}}
}

// collectGateSlow raises the gate-slow items of every rig the run log names. An
// unreadable log is the error, so collectAttention keeps the previous items and
// logs it, rather than reading the log as no slow gate.
func (s *attentionSources) collectGateSlow(_ context.Context) ([]attention.Item, error) {
	runs, _, err := land.ReadGateRuns(s.d.config.TownRoot)
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	seen := make(map[string]bool)
	for _, r := range runs {
		if !seen[r.Rig] {
			seen[r.Rig] = true
			out = append(out, gateSlowItems(runs, r.Rig)...)
		}
	}
	return out, nil
}

// gateSlowItem picks the gate-slow items out of the previous items a failed read keeps.
func gateSlowItem(it attention.Item) bool {
	return strings.HasPrefix(it.Key, gateSlowPrefix)
}
