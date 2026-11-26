package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// gateThrottleWindow is how many of a rig's newest gate runs must all carry the
// recorder's not-read marker before the queue says the runner's throttling is
// unknown (gt-38268i).
const (
	gateThrottleWindow = 3
	gateThrottlePrefix = "gate-throttle-unread:" // keys every gate-throttle-unread item
)

// gateThrottleOwned picks the gate-throttle-unread items out of the previous
// items a failed read keeps: the kind and the key prefix both have to match
// (gt-hnd1kj).
func gateThrottleOwned(it attention.Item) bool {
	return it.Kind == attention.KindGateThrottleUnread && strings.HasPrefix(it.Key, gateThrottlePrefix)
}

// gateThrottleItems returns rig's gate-throttle-unread item, or none. It holds
// when the rig's last gateThrottleWindow runs all carry RunnerPeriods below
// zero, the recorder's marker for a throttle reading it did not take; a zero
// is a reading of no throttle. runs is in log order, oldest first.
func gateThrottleItems(runs []land.GateRun, rig string) []attention.Item {
	var mine []land.GateRun
	for _, r := range runs {
		if r.Rig == rig {
			mine = append(mine, r)
		}
	}
	if len(mine) < gateThrottleWindow {
		return nil
	}
	for _, r := range mine[len(mine)-gateThrottleWindow:] {
		if r.RunnerPeriods >= 0 {
			return nil
		}
	}
	return []attention.Item{{
		Kind:     attention.KindGateThrottleUnread,
		Key:      gateThrottlePrefix + rig,
		Severity: attention.SeverityMedium,
		Rig:      rig,
		Summary: fmt.Sprintf("%s: the last %d gate runs could not read the runner throttle",
			rig, gateThrottleWindow),
	}}
}

// collectGateThrottle raises the gate-throttle-unread items of every rig the run
// log names. An unreadable log is the error, so collectAttention keeps the
// previous items and logs it, rather than reading the log as a rig whose last
// runs read the throttle (gt-oe1mqo).
func (s *attentionSources) collectGateThrottle(_ context.Context) ([]attention.Item, error) {
	runs, _, err := land.ReadGateRuns(s.d.config.TownRoot)
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	seen := make(map[string]bool)
	for _, r := range runs {
		if !seen[r.Rig] {
			seen[r.Rig] = true
			out = append(out, gateThrottleItems(runs, r.Rig)...)
		}
	}
	return out, nil
}
