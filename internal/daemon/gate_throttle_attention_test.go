package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// gateThrottleRun is one run of rig whose runner throttle counters are the
// recorder's marker: -1 when the reading was not taken, else the real count.
func gateThrottleRun(rig string, periods int64) land.GateRun {
	return land.GateRun{Rig: rig, RunnerPeriods: periods}
}

func TestGateThrottleItems(t *testing.T) {
	t.Parallel()
	unread := gateThrottleRun("gastown", -1)
	read := gateThrottleRun("gastown", 40)
	zero := gateThrottleRun("gastown", 0)
	otherRig := gateThrottleRun("beads", -1)
	cases := []struct {
		name  string
		rig   string
		runs  []land.GateRun
		items int
	}{
		{"three unread runs raise one item", "gastown", []land.GateRun{unread, unread, unread}, 1},
		{"two unread runs raise none", "gastown", []land.GateRun{unread, unread}, 0},
		{"unread, unread, read raise none", "gastown", []land.GateRun{unread, unread, read}, 0},
		{"read, unread, unread, unread raise one item", "gastown", []land.GateRun{read, unread, unread, unread}, 1},
		{"only the last three runs count", "gastown", []land.GateRun{unread, read, unread, unread, unread}, 1},
		{"an unread run before a read newest three raises none", "gastown", []land.GateRun{unread, unread, unread, read}, 0},
		{"a zero reading is a reading of no throttle", "gastown", []land.GateRun{unread, unread, zero}, 0},
		{"another rig's runs do not count", "gastown", []land.GateRun{unread, unread, otherRig}, 0},
		{"the item is keyed by the rig it reads", "beads", []land.GateRun{otherRig, otherRig, otherRig}, 1},
		{"no runs raise none", "gastown", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gateThrottleItems(tc.runs, tc.rig)
			if len(got) != tc.items {
				t.Fatalf("gateThrottleItems returned %d items, want %d: %+v", len(got), tc.items, got)
			}
			if tc.items == 0 {
				return
			}
			it := got[0]
			if it.Kind != attention.KindGateThrottleUnread {
				t.Errorf("item %+v kind %q, want %q", it, it.Kind, attention.KindGateThrottleUnread)
			}
			if it.Key != "gate-throttle-unread:"+tc.rig || it.Severity != attention.SeverityMedium {
				t.Errorf("item key %q severity %q, want gate-throttle-unread:%s medium", it.Key, it.Severity, tc.rig)
			}
			if it.Rig != tc.rig {
				t.Errorf("item rig %q, want %q", it.Rig, tc.rig)
			}
			if !strings.Contains(it.Summary, tc.rig) || !strings.Contains(it.Summary, fmt.Sprintf("%d", gateThrottleWindow)) {
				t.Errorf("summary %q does not name rig %q and window %d", it.Summary, tc.rig, gateThrottleWindow)
			}
		})
	}
}
