package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// TestDashboardLandingsPause: the Landings pane names the landing hold the
// health report carries, and never reads a report it cannot trust as clear
// (gt-ckc93z).
func TestDashboardLandingsPause(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, tc := range []struct {
		name   string
		write  bool
		report townhealth.Report
		want   *dashboard.LandingsPause
	}{
		{
			name: "no health report is unknown, not clear",
			want: &dashboard.LandingsPause{Unknown: true},
		},
		{
			name:   "a fresh report with a pause names the reason",
			write:  true,
			report: townhealth.Report{At: now, Verdict: townhealth.Green, LandingPaused: "overseer: drain"},
			want:   &dashboard.LandingsPause{Reason: "overseer: drain"},
		},
		{
			name:   "a fresh report with no pause is clear",
			write:  true,
			report: townhealth.Report{At: now, Verdict: townhealth.Green},
		},
		{
			name:   "a stale report with no pause is unknown",
			write:  true,
			report: townhealth.Report{At: now.Add(-2 * townhealth.DefaultStaleAfter), Verdict: townhealth.Green},
			want:   &dashboard.LandingsPause{Unknown: true},
		},
		{
			name:   "a stale report that carried a pause still names it",
			write:  true,
			report: townhealth.Report{At: now.Add(-2 * townhealth.DefaultStaleAfter), Verdict: townhealth.Green, LandingPaused: "overseer: drain"},
			want:   &dashboard.LandingsPause{Reason: "overseer: drain"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.write {
				if err := townhealth.Write(root, tc.report); err != nil {
					t.Fatal(err)
				}
			}
			got := dashboardLandingsPause(root, now)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("dashboardLandingsPause = %+v, want %+v", got, tc.want)
			}
		})
	}
}
