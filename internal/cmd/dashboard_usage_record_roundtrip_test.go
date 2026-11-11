package cmd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// A record usageattrib.Write puts in the scan's records directory is the file the
// panel reads: the reader decodes it, and the day's and the month's folds count it.
func TestUsageReaderReadsARecordWrittenByUsageattrib(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	end := usageReaderNow.Add(-time.Hour)
	seat := "gastown/polecats/dag"
	facts := usageattrib.Facts{
		Bead:      "gt-abc12",
		Kind:      dashboard.UsageKindWork,
		Class:     "polecat",
		Rig:       "gastown",
		Seat:      &seat,
		Workspace: "ws-a",
		Start:     end.Add(-2 * time.Minute),
		End:       end,
		Complete:  true,
	}
	// Wall time is the span of the session's own message timestamps, so the two calls
	// sit 34 seconds apart. Uncached is input + cache creation + output: 70 + 11.
	first, second := "m1", "m2"
	sess := usageattrib.SessionUsage{
		ID:       "s-1",
		Complete: true,
		Main: []usageattrib.Message{
			{ID: &first, Model: "claude-opus-5-5", TS: end.Add(-time.Minute).UTC().Format(time.RFC3339),
				Input: 10, CacheCreation: 20, CacheRead: 30, Output: 40},
			{ID: &second, Model: "claude-opus-5-5", TS: end.Add(-time.Minute + 34*time.Second).UTC().Format(time.RFC3339),
				Input: 5, Output: 6},
		},
	}
	rec := usageattrib.Build(facts, []usageattrib.SessionUsage{sess})
	const wantCalls = 2
	const wantUncached = 81
	const wantWallS = 34

	if err := usageattrib.Write(filepath.Dir(records), rec, []string{"s-1"}); err != nil {
		t.Fatalf("usageattrib.Write: %v", err)
	}
	path := filepath.Join(records, "gt-abc12.work.json")
	setUsageMtime(t, path, end)

	r := newDashUsageReader(root)
	u := r.read(usageReaderNow)
	if u.Unavailable {
		t.Fatalf("the panel read came back unavailable: %+v", u)
	}
	if u.Skipped != 0 {
		t.Errorf("skipped = %d, want the written record read, not skipped", u.Skipped)
	}
	if len(u.TopBeads) != 1 || u.TopBeads[0].Bead != "gt-abc12" || u.TopBeads[0].WallS != wantWallS {
		t.Fatalf("top beads = %+v, want gt-abc12 with wall %d", u.TopBeads, wantWallS)
	}
	var calls, uncached int64
	for _, row := range u.ByClassModel {
		calls += int64(row.Calls)
		uncached += row.Uncached
	}
	if calls != wantCalls || uncached != wantUncached {
		t.Errorf("by class and model sums to %d calls, %d uncached; want %d, %d", calls, uncached, wantCalls, wantUncached)
	}

	month := r.monthRecords(usageReaderNow)
	if len(month) != 1 || month[0].Bead != "gt-abc12" || month[0].Totals.Uncached != wantUncached {
		t.Errorf("the plan's month read = %+v, want the one written record with %d uncached", month, wantUncached)
	}
}
