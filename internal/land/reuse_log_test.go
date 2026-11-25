package land

import (
	"context"
	"testing"
	"time"
)

func TestVerdictReuseSuffixForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cost time.Duration
		age  time.Duration
		want string
	}{
		{"minutes and seconds", 2*time.Minute + 31*time.Second, 14 * time.Minute, ", om reused (2m31s, 14m0s old)"},
		{"zero durations", 0, 0, ", om reused (0s, 0s old)"},
		{"negative age keeps its sign", time.Second, -5 * time.Minute, ", om reused (1s, -5m0s old)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := omReusedSuffix(tc.cost, tc.age); got != tc.want {
				t.Fatalf("omReusedSuffix = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVerdictReuseSuffixFormStageLine(t *testing.T) {
	t.Parallel()
	noLoad := func() (float64, bool) { return 0, false }
	got := stageLine([]string{"gate 3s"}, 0, false, noLoad) + omReusedSuffix(2*time.Minute+31*time.Second, 14*time.Minute)
	want := "stages: gate 3s, om reused (2m31s, 14m0s old)"
	if got != want {
		t.Fatalf("stage line = %q, want %q", got, want)
	}
}

func TestVerdictReuseApplyBar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      Verdict
		bar     float64
		want    string
		wantSum string
	}{
		{"approve above the bar stays", Verdict{Verdict: VerdictApprove, Score: 0.9, Summary: "fine"}, 0.8, VerdictApprove, "fine"},
		{"approve at the bar stays", Verdict{Verdict: VerdictApprove, Score: 0.8, Summary: "fine"}, 0.8, VerdictApprove, "fine"},
		{"approve below the bar is request_changes", Verdict{Verdict: VerdictApprove, Score: 0.5, Summary: "fine"}, 0.8, VerdictRequestChanges, "score 0.50 is below the rig's .om.json threshold 0.80. fine"},
		{"request_changes is unchanged", Verdict{Verdict: VerdictRequestChanges, Score: 0.2, Summary: "no"}, 0.8, VerdictRequestChanges, "no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := applyBar(tc.in, tc.bar)
			if got.Verdict != tc.want || got.Summary != tc.wantSum {
				t.Fatalf("applyBar = %q %q, want %q %q", got.Verdict, got.Summary, tc.want, tc.wantSum)
			}
		})
	}
}

// TestVerdictReuseApplyBarMatchesReviewOnce is the drift pin: the same approve
// below the bar gives the same verdict and summary through reviewOnce, which
// applies the bar to a fresh om run, and through applyBar, which a reused
// verdict takes.
func TestVerdictReuseApplyBarMatchesReviewOnce(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.5,"verdict":"approve","summary":"looks fine"}`, 0, nil, &argv)
	inline, err := reviewAt(context.Background(), r, `{"threshold": 0.8}`, "/wt", "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if inline.Verdict != VerdictRequestChanges {
		t.Fatalf("reviewOnce verdict = %q, want %q: the scenario is not below the bar", inline.Verdict, VerdictRequestChanges)
	}
	reused := applyBar(Verdict{Verdict: VerdictApprove, Score: 0.5, Summary: "looks fine"}, 0.8)
	if reused.Verdict != inline.Verdict || reused.Summary != inline.Summary {
		t.Fatalf("applyBar = %q %q, reviewOnce = %q %q", reused.Verdict, reused.Summary, inline.Verdict, inline.Summary)
	}
}
