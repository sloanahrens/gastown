package quota

import (
	"testing"
	"time"
)

func TestPlanResume(t *testing.T) {
	// Fixed reference instant: 2026-09-09 17:04:30 America/Chicago.
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("loading America/Chicago: %v", err)
	}
	now := time.Date(2026, 9, 9, 17, 4, 30, 0, loc)

	cases := []struct {
		name    string
		scanned []ScanResult
		want    []string // expected candidate session names, in order
	}{
		{
			name:    "no sessions",
			scanned: nil,
			want:    nil,
		},
		{
			name: "not rate limited is not a candidate",
			scanned: []ScanResult{
				{Session: "gt-agate", RateLimited: false, ResetsAt: "5pm (America/Chicago)"},
			},
			want: nil,
		},
		{
			name: "rate limited but no parsed reset time is not a candidate",
			scanned: []ScanResult{
				{Session: "gt-agate", RateLimited: true, ResetsAt: ""},
			},
			want: nil,
		},
		{
			name: "rate limited with unparseable reset text is not a candidate",
			scanned: []ScanResult{
				{Session: "gt-agate", RateLimited: true, ResetsAt: "sometime soon"},
			},
			want: nil,
		},
		{
			name: "reset time still in the future is not a candidate",
			scanned: []ScanResult{
				{Session: "gt-agate", RateLimited: true, ResetsAt: "6pm (America/Chicago)"},
			},
			want: nil,
		},
		{
			name: "reset time within the grace window is not yet a candidate",
			scanned: []ScanResult{
				// Reset was at 17:04:30, 30s ago — inside the 1-minute grace.
				{Session: "gt-agate", RateLimited: true, ResetsAt: "5:04pm (America/Chicago)"},
			},
			want: nil,
		},
		{
			name: "reset time past the grace window is a candidate",
			scanned: []ScanResult{
				// Reset was at 5pm, well past the 1-minute grace.
				{Session: "gt-agate", RateLimited: true, ResetsAt: "5pm (America/Chicago)"},
			},
			want: []string{"gt-agate"},
		},
		{
			name: "mixed sessions only surface the one past reset",
			scanned: []ScanResult{
				{Session: "gt-agate", RateLimited: true, ResetsAt: "5pm (America/Chicago)"},   // past
				{Session: "gt-obsidian", RateLimited: true, ResetsAt: "6pm (America/Chicago)"}, // future
				{Session: "gt-onyx", RateLimited: false},                                       // not limited
			},
			want: []string{"gt-agate"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanResume(tc.scanned, now)
			if len(got) != len(tc.want) {
				t.Fatalf("PlanResume() returned %d candidates, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, session := range tc.want {
				if got[i].Session != session {
					t.Errorf("candidate[%d].Session = %q, want %q", i, got[i].Session, session)
				}
				if got[i].ResetTime.After(now) {
					t.Errorf("candidate[%d].ResetTime = %v, must not be after now (%v)", i, got[i].ResetTime, now)
				}
			}
		})
	}
}

// TestPlanResume_DisabledInstrumentCatch guards against a fix that "clears"
// the reported symptom by never nudging anyone — e.g. an accidental
// inverted comparison that always skips. A realistic past-reset fixture
// (not a trivially empty one) must still produce a candidate.
func TestPlanResume_DisabledInstrumentCatch(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2026, 9, 9, 17, 30, 0, 0, loc)

	scanned := []ScanResult{
		{
			Session:       "gt-refinery",
			AccountHandle: "work",
			ConfigDir:     "/home/user/.claude-work",
			RateLimited:   true,
			MatchedLine:   "You've hit your session limit · resets 5pm (America/Chicago)",
			ResetsAt:      "5pm (America/Chicago)",
		},
	}

	got := PlanResume(scanned, now)
	if len(got) != 1 || got[0].Session != "gt-refinery" {
		t.Fatalf("PlanResume() = %+v, want exactly one candidate for gt-refinery", got)
	}
}
