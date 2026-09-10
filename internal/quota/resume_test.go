package quota

import (
	"testing"
	"time"
)

// TestScanAndPlanResume_NoAccountsConfigured is the gt-749e acceptance test:
// a session showing "resets 5pm" with now = 5:01pm gets exactly one resume
// nudge candidate; with now = 4:59pm it gets none — and this holds with NO
// accounts.json (accounts passed as nil to NewScanner, the situation on a
// single-account town where 'gt account add' was never run).
func TestScanAndPlanResume_NoAccountsConfigured(t *testing.T) {
	setupTestRegistry(t)

	tmux := &mockTmux{
		sessions: []string{"gt-agate"},
		paneContent: map[string]string{
			"gt-agate": "You've hit your session limit · resets 5pm (America/Chicago)",
		},
	}

	// No accounts config at all — this is the "no accounts.json" case.
	scanner, err := NewScanner(tmux, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := scanner.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].RateLimited {
		t.Fatalf("expected one rate-limited scan result, got %+v", results)
	}

	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("loading America/Chicago: %v", err)
	}

	// Before reset (plus grace): no candidate yet.
	before := time.Date(2026, 9, 9, 16, 59, 0, 0, loc)
	if got := PlanResume(results, before); len(got) != 0 {
		t.Errorf("PlanResume() at 4:59pm = %+v, want no candidates", got)
	}

	// After reset (plus grace): exactly one candidate.
	after := time.Date(2026, 9, 9, 17, 1, 0, 0, loc)
	got := PlanResume(results, after)
	if len(got) != 1 || got[0].Session != "gt-agate" {
		t.Fatalf("PlanResume() at 5:01pm = %+v, want exactly one candidate for gt-agate", got)
	}
}

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
