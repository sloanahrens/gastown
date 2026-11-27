package sandboxstuck

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

var t0 = time.Date(2026, 11, 25, 6, 20, 0, 0, time.UTC)

func checkStuck(t *testing.T, tr *Tracker, now time.Time, want []Finding) {
	t.Helper()
	if got := tr.Stuck(now); !slices.Equal(got, want) {
		t.Fatalf("Stuck(%v) = %+v, want %+v", now, got, want)
	}
}

func TestStuckAtThirtyMinutes(t *testing.T) {
	t.Parallel()
	var tr Tracker
	tr.Observe([]Sandbox{{"b-1", "Provisioning"}, {"a-1", "Provisioning"}}, t0)
	checkStuck(t, &tr, t0.Add(29*time.Minute), nil)
	checkStuck(t, &tr, t0.Add(After), []Finding{{"a-1", "Provisioning", After}, {"b-1", "Provisioning", After}})
}

func TestStuckClockRestartsAndForgets(t *testing.T) {
	t.Parallel()
	var tr Tracker
	tr.Observe([]Sandbox{{"a", "Provisioning"}, {"b", "Provisioning"}, {"c", "Provisioning"}}, t0)
	tr.Observe([]Sandbox{{"a", "Installing"}, {"b", "Ready"}, {"c", "Provisioning"}}, t0.Add(20*time.Minute))
	checkStuck(t, &tr, t0.Add(30*time.Minute), []Finding{{"c", "Provisioning", 30 * time.Minute}})
	tr.Observe([]Sandbox{{"a", "Installing"}, {"b", "Provisioning"}}, t0.Add(40*time.Minute))
	checkStuck(t, &tr, t0.Add(50*time.Minute), []Finding{{"a", "Installing", 30 * time.Minute}})
	tr.Observe([]Sandbox{{"a", "Installing"}}, t0.Add(-time.Hour))
	checkStuck(t, &tr, t0.Add(-time.Hour+After), []Finding{{"a", "Installing", After}})
}

func TestStuckIgnoredPhases(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"", " ", "ready", " Ready ", "RUNNING", "Error", "failed"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			var tr Tracker
			tr.Observe([]Sandbox{{"sb", phase}, {"  ", "Provisioning"}}, t0)
			checkStuck(t, &tr, t0.Add(After), nil)
		})
	}
}
