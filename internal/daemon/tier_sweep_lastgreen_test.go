package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// TestTierSweepLastGreenAt: LastGreenAt is the finish time of the cycle that set
// LastGreenSHA. A fully green cycle moves the pair together, a red or Partial
// cycle leaves it, and a record from before the field reads as zero (gt-igrpge).
func TestTierSweepLastGreenAt(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	tip := ""
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return tip, nil }
	// The clock moves inside every cycle, before its stages run, so a LastGreenAt
	// taken from the cycle's start rather than its finish is red, and a later
	// cycle's clock is never the value the green one recorded.
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) {
		d.clock.(*clockwork.FakeClock).Advance(time.Minute)
		return func() {}, nil
	}
	green := &tierSweepRunRecorder{}
	green.green()
	partial := func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{} // never started, so the cycle defers
		}
		return green.result(st)
	}
	finish := now.Add(time.Minute)
	tests := []struct {
		name   string
		tip    string
		old    bool                                      // seed a record written before the field existed
		result func(tierSweepStage) tierSweepStageResult // nil leaves the recorder's own: a RED cycle
		want   string
		at     time.Time
	}{
		{"a fully green cycle sets it", "green1", false, green.result, "green1", finish},
		{"a red cycle leaves it", "red1", false, nil, "green1", finish},
		{"a Partial cycle leaves it", "part1", false, partial, "green1", finish},
		{"a record from before the field loads as zero", "red2", true, nil, "green0", time.Time{}},
	}
	for _, tc := range tests {
		tip, rec.result = tc.tip, tc.result
		if tc.old {
			writeFile(t, tierSweepStatePath(d.config.TownRoot, "gastown"), `{"last_run":"2026-11-16T11:30:00Z","last_sha":"old","last_green_sha":"green0"}`)
		}
		d.runTierSweep()
		if st := tierSweepTestState(t, d, "gastown"); st.LastSHA != tc.tip || st.LastGreenSHA != tc.want || !st.LastGreenAt.Equal(tc.at) {
			t.Errorf("%s: LastGreenSHA, LastGreenAt = %q, %v, want %q, %v", tc.name, st.LastGreenSHA, st.LastGreenAt, tc.want, tc.at)
		}
	}
}
