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

func TestGateThrottleCollect(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	// Two rigs with enough unread runs raise one item each, however many rows
	// name them; a rig with two unread runs raises none.
	runs := []land.GateRun{
		gateThrottleRun("gastown", -1),
		gateThrottleRun("beads", -1),
		gateThrottleRun("nux", -1),
		gateThrottleRun("gastown", -1),
		gateThrottleRun("beads", -1),
		gateThrottleRun("nux", -1),
		gateThrottleRun("gastown", -1),
		gateThrottleRun("beads", -1),
		gateThrottleRun("beads", -1),
		gateThrottleRun("beads", -1),
	}
	for _, r := range runs {
		if err := land.AppendGateRun(f.d.config.TownRoot, r); err != nil {
			t.Fatalf("appending a %s gate run: %v", r.Rig, err)
		}
	}

	got, err := f.src.collectGateThrottle(context.Background())
	if err != nil {
		t.Fatalf("collectGateThrottle: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("collectGateThrottle returned %d items, want one per unread rig (gastown, beads): %+v", len(got), got)
	}
	want := map[string]bool{"gate-throttle-unread:gastown": true, "gate-throttle-unread:beads": true}
	for _, it := range got {
		if !want[it.Key] {
			t.Errorf("unexpected item %q: %+v", it.Key, it)
		}
		delete(want, it.Key)
		if it.Kind != attention.KindGateThrottleUnread {
			t.Errorf("item %+v kind %q, want %q", it, it.Kind, attention.KindGateThrottleUnread)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing items %v", want)
	}
}

func TestGateThrottleCollectFailedRead(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	// A directory where the log file belongs: the open succeeds and the scan fails.
	if err := os.MkdirAll(filepath.Join(f.d.config.TownRoot, ".runtime", "metrics", "gate-runs.jsonl"), 0o755); err != nil {
		t.Fatalf("making the unreadable log: %v", err)
	}
	if _, err := f.src.collectGateThrottle(context.Background()); err == nil {
		t.Fatal("collectGateThrottle read an unreadable log without an error")
	}
	prev := attention.State{Items: []attention.Item{
		{Kind: attention.KindGateThrottleUnread, Key: "gate-throttle-unread:gastown", Severity: attention.SeverityMedium, Summary: "previous gate-throttle"},
		{Kind: attention.KindGateSlow, Key: "gate-slow:gastown", Severity: attention.SeverityMedium, Summary: "another collector's item"},
	}}
	var logs strings.Builder
	logf := func(format string, args ...any) { fmt.Fprintf(&logs, format+"\n", args...) }
	got := collectAttention(context.Background(), prev, []attentionCollector{{kind: attention.KindGateThrottleUnread, collect: f.src.collectGateThrottle}}, logf)

	if len(got) != 1 || got[0].Summary != "previous gate-throttle" {
		t.Fatalf("a failed read kept %+v, want only the previous gate-throttle item", got)
	}
	if !strings.Contains(logs.String(), "land: reading") {
		t.Errorf("the failed read is not logged; log: %q", logs.String())
	}
}
