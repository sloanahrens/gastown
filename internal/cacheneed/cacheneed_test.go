package cacheneed

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jobledger"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestCompute: runs of three jobs give a sorted row each with the exact maxima,
// the latest run's cap and the need; failed, old and young-less runs count for
// nothing; an absent cap or claim stays absent (gt-l603qd).
func TestCompute(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	at := func(days int) time.Time { return since.AddDate(0, 0, days) }
	run := func(job, outcome string, end time.Time, stats map[string]int64) jobledger.Run {
		return jobledger.Run{Job: job, Outcome: outcome, EndedAt: end, Stats: stats}
	}
	ptr := func(v int64) *int64 { return &v }
	runs := []jobledger.Run{
		run("beta", "ok", at(1), map[string]int64{"young_bytes": 999, "claim_bytes_before": 5000, "cap_bytes": 2000}),
		run("alpha", "ok", at(3), map[string]int64{"young_bytes": 300, "claim_bytes_before": 900, "cap_bytes": 1000}),
		run("beta", "failed", at(4), map[string]int64{"young_bytes": 99999, "claim_bytes_before": 99999, "cap_bytes": 99999}),
		run("alpha", "ok", at(1), map[string]int64{"young_bytes": 400, "claim_bytes_before": 700, "cap_bytes": 800}),
		run("gamma", "ok", at(2), map[string]int64{"young_bytes": 50}),
		run("alpha", "ok", at(5), map[string]int64{"young_bytes": 200, "claim_bytes_before": 950}),
		run("beta", "ok", at(0).Add(-time.Hour), map[string]int64{"young_bytes": 99999, "cap_bytes": 1}),
		run("gamma", "ok", at(2), map[string]int64{"claim_bytes_before": 1, "cap_bytes": 1}),
		run("delta", "ok", at(2), map[string]int64{"claim_bytes_before": 7}),
	}
	got, err := Compute(runs, since, 10)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	want := []Row{
		{Job: "alpha", Runs: 3, MaxYoungBytes: 400, MaxClaimBytesBefore: ptr(950), Need: 440, FirstEnd: at(1), LastEnd: at(5)},
		{Job: "beta", Runs: 1, MaxYoungBytes: 999, MaxClaimBytesBefore: ptr(5000), CapBytes: ptr(2000), Need: 1099, FirstEnd: at(1), LastEnd: at(1)},
		{Job: "gamma", Runs: 1, MaxYoungBytes: 50, Need: 55, FirstEnd: at(2), LastEnd: at(2)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Compute = %+v, want %+v", got, want)
	}
}

// TestComputeHeadroomBounds: a headroom below 0 or above 200 percent is refused,
// 0 and 200 are accepted (gt-l603qd).
func TestComputeHeadroomBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pct     int
		wantErr bool
	}{{-1, true}, {201, true}, {0, false}, {200, false}} {
		if _, err := Compute(nil, time.Time{}, tc.pct); (err != nil) != tc.wantErr {
			t.Errorf("headroom %d%%: err = %v, wantErr %v", tc.pct, err, tc.wantErr)
		}
	}
}
