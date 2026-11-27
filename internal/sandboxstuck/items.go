package sandboxstuck

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// Items returns one seat-sandbox-stale item per sandbox Stuck reports at now, keyed by keyPrefix and the sandbox name.
func (t *Tracker) Items(now time.Time, keyPrefix string) []attention.Item {
	var items []attention.Item
	for _, f := range t.Stuck(now) {
		items = append(items, attention.Item{
			Key:       keyPrefix + f.Name,
			Kind:      attention.KindSeatSandboxStale,
			Severity:  attention.SeverityHigh,
			Summary:   stuckSummary(f),
			FirstSeen: now.Add(-f.Age),
			LastSeen:  now,
		})
	}
	return items
}

// stuckSummary carries no delete command: the phase is only the gateway's report, so the pod may still run and the operator must judge it first.
func stuckSummary(f Finding) string {
	return fmt.Sprintf("sandbox %s has been in phase %s for %d min; the phase is the gateway's report and the pod may still be running. Read the Sandbox CR, the sandbox pod and the os-supervisor pod before judging it; no daemon rule ends a sandbox in this phase. Run gt polecat check-recovery and read docs/sandbox-rescue-runbook.md, then delete the sandbox only after its unsaved work is saved.",
		f.Name, f.Phase, int64(f.Age/time.Minute))
}
