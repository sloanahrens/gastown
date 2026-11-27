package sandboxstuck

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// stuckSince returns a tracker that saw name in phase at t0 and nothing else.
func stuckSince(name, phase string) *Tracker {
	var tr Tracker
	tr.Observe([]Sandbox{{name, phase}}, t0)
	return &tr
}

func TestItemsForAStuckSandbox(t *testing.T) {
	t.Parallel()
	now := t0.Add(47*time.Minute + 30*time.Second)
	items := stuckSince("a-1", "Provisioning").Items(now, "seat-sandbox-stale:")
	if len(items) != 1 {
		t.Fatalf("Items = %d, want 1: %+v", len(items), items)
	}
	it := items[0]
	if it.Key != "seat-sandbox-stale:a-1" || it.Kind != attention.KindSeatSandboxStale || it.Severity != attention.SeverityHigh {
		t.Fatalf("item = key %q kind %q severity %q, want seat-sandbox-stale:a-1 seat-sandbox-stale high", it.Key, it.Kind, it.Severity)
	}
	if !it.FirstSeen.Equal(t0) || !it.LastSeen.Equal(now) {
		t.Errorf("FirstSeen, LastSeen = %v, %v; want %v, %v", it.FirstSeen, it.LastSeen, t0, now)
	}
	for _, want := range []string{"a-1", "Provisioning", "47 min", "gateway's report", "pod may still be running",
		"Sandbox CR", "sandbox pod", "os-supervisor pod", "gt polecat check-recovery", "docs/sandbox-rescue-runbook.md"} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("Summary = %q, want it to contain %q", it.Summary, want)
		}
	}
}

func TestItemsSummaryPrintsNoDeleteCommand(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"Provisioning", "Installing", "Pending"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			items := stuckSince("a-1", phase).Items(t0.Add(After), "seat-sandbox-stale:")
			if len(items) != 1 {
				t.Fatalf("Items = %d, want 1", len(items))
			}
			if strings.Contains(items[0].Summary, "sandbox delete") {
				t.Errorf("Summary = %q, must not print a delete command", items[0].Summary)
			}
		})
	}
}

func TestItemsNoneBeforeThirtyMinutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		elapsed   time.Duration
		wantItems int
	}{
		{"29 minutes", 29 * time.Minute, 0},
		{"30 minutes", After, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			items := stuckSince("a-1", "Provisioning").Items(t0.Add(tc.elapsed), "seat-sandbox-stale:")
			if len(items) != tc.wantItems {
				t.Fatalf("Items = %d, want %d: %+v", len(items), tc.wantItems, items)
			}
		})
	}
}

func TestItemsOnePerStuckSandbox(t *testing.T) {
	t.Parallel()
	var tr Tracker
	tr.Observe([]Sandbox{{"b-1", "Provisioning"}, {"a-1", "Installing"}}, t0)
	items := tr.Items(t0.Add(After), "seat-sandbox-stale:")
	if len(items) != 2 || items[0].Key != "seat-sandbox-stale:a-1" || items[1].Key != "seat-sandbox-stale:b-1" {
		t.Fatalf("Items = %+v, want one item each for a-1 then b-1", items)
	}
}
