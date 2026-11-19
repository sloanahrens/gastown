package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// gateSlowRun is one run of rig, over its tier bar or not, with its tier and pole seconds.
func gateSlowRun(rig string, over bool, tierS, poleS float64) land.GateRun {
	return land.GateRun{Rig: rig, Over: over, TierS: tierS, TierBarS: 300, PolePkg: "./internal/daemon", PoleS: poleS}
}

func TestGateSlowAttentionItems(t *testing.T) {
	t.Parallel()
	over := gateSlowRun("gastown", true, 400, 120)
	clean := gateSlowRun("gastown", false, 200, 40)
	pole := gateSlowRun("gastown", false, 200, 90)
	edge := gateSlowRun("gastown", false, 200, 80)
	noTier := gateSlowRun("gastown", false, 0, 90)
	beads := gateSlowRun("beads", true, 400, 120)
	cases := []struct {
		name  string
		rig   string
		runs  []land.GateRun
		items int
	}{
		{"three over-bar runs raise one item", "gastown", []land.GateRun{over, over, over}, 1},
		{"two over-bar runs raise none", "gastown", []land.GateRun{over, over}, 0},
		{"a clean newest run raises none", "gastown", []land.GateRun{over, over, clean}, 0},
		{"three pole-bound runs raise one item", "gastown", []land.GateRun{pole, pole, pole}, 1},
		{"a pole share of exactly 0.4 is not pole-bound", "gastown", []land.GateRun{edge, edge, edge}, 0},
		{"a run with no tier seconds is not pole-bound", "gastown", []land.GateRun{noTier, noTier, noTier}, 0},
		{"over-bar and pole-bound runs mixed raise none", "gastown", []land.GateRun{over, pole, over}, 0},
		{"only the last three runs count", "gastown", []land.GateRun{clean, over, over, over}, 1},
		{"an over-bar run before a clean newest three raises none", "gastown", []land.GateRun{over, over, over, clean}, 0},
		{"another rig's runs do not count", "gastown", []land.GateRun{over, over, beads}, 0},
		{"the item is keyed by the rig it reads", "beads", []land.GateRun{beads, beads, beads}, 1},
		{"no runs raise none", "gastown", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gateSlowItems(tc.runs, tc.rig)
			if len(got) != tc.items {
				t.Fatalf("gateSlowItems returned %d items, want %d: %+v", len(got), tc.items, got)
			}
			if tc.items == 0 {
				return
			}
			newest := tc.runs[len(tc.runs)-1]
			it := got[0]
			if it.Key != "gate-slow:"+tc.rig || it.Severity != attention.SeverityMedium {
				t.Errorf("item key %q severity %q, want gate-slow:%s medium", it.Key, it.Severity, tc.rig)
			}
			if !strings.Contains(it.Summary, newest.PolePkg) || !strings.Contains(it.Summary, fmt.Sprintf("tier %.0fs", newest.TierS)) {
				t.Errorf("summary %q does not name pole %q and tier %.0fs", it.Summary, newest.PolePkg, newest.TierS)
			}
		})
	}
}

func TestGateSlowAttentionFailedRead(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	// A directory where the log file belongs: the open succeeds and the scan fails.
	if err := os.MkdirAll(filepath.Join(f.d.config.TownRoot, ".runtime", "metrics", "gate-runs.jsonl"), 0o755); err != nil {
		t.Fatalf("making the unreadable log: %v", err)
	}
	prev := attention.State{Items: []attention.Item{
		{Key: "gate-slow:gastown", Severity: attention.SeverityMedium, Summary: "previous gate-slow"},
		{Key: "stall:gastown/opal", Severity: attention.SeverityHigh, Summary: "another collector's item"},
	}}
	var logs strings.Builder
	logf := func(format string, args ...any) { fmt.Fprintf(&logs, format+"\n", args...) }
	got := collectAttention(context.Background(), prev, []attentionCollector{{collect: f.src.collectGateSlow, owns: gateSlowItem}}, logf)

	if len(got) != 1 || got[0].Summary != "previous gate-slow" {
		t.Fatalf("a failed read kept %+v, want only the previous gate-slow item", got)
	}
	if !strings.Contains(logs.String(), "land: reading") {
		t.Errorf("the failed read is not logged; log: %q", logs.String())
	}
}
