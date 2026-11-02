package specdispatch

import "testing"

// The fence row is the case the hint is for: a bead pastes its failing output.
func TestSlowTextHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"timeout", "the request hit a timeout", true},
		{"timed out", "TestFoo timed out on CI", true},
		{"timed-out", "a timed-out request", true},
		{"time-out", "a time-out occurred", true},
		{"time-outs", "two time-outs in a row", true},
		{"times out", "the suite times out on CI", true},
		{"slow", "this is a slow test", true},
		{"uppercase", "TIMEOUT while building", true},
		{"seconds limit", "the lease is 600s", true},
		{"takes N minutes", "the suite takes 12 minutes", true},
		{"N min", "allow 5 min", true},
		{"ten minute limit", "the limit is 10m", true},
		{"trigger in a fence", "```\npanic: test timed out after 10m0s\n```", true},
		{"slowly varying", "slowly varying input", false},
		{"timeline", "the timeline of events", false},
		{"sleep removed", "the sleep is removed", false},
		{"metres", "a wall 3 m tall", false},
		{"metres decimal", "the 2.5 m board", false},
		{"metres per second", "run at 10 m/s", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SlowTextHint(tt.text); got != tt.want {
				t.Errorf("SlowTextHint(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
