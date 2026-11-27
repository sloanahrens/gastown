package daemon

import (
	"testing"
	"time"
)

// TestClusterDriftStartDelayIsThreeMinutes pins the start delay the drift
// patrol waits after the daemon starts (gt-myyr6e).
func TestClusterDriftStartDelayIsThreeMinutes(t *testing.T) {
	t.Parallel()

	if clusterDriftStartDelay != 3*time.Minute {
		t.Fatalf("clusterDriftStartDelay = %v, want 3m0s", clusterDriftStartDelay)
	}
}

// TestClusterDriftStartingFollowsTheRecordedStart: the patrol is starting
// just before three minutes after the recorded start, and is no longer starting
// from three minutes on (gt-myyr6e).
func TestClusterDriftStartingFollowsTheRecordedStart(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)
	d := &Daemon{}
	d.clusterDriftCache.started = started

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"one nanosecond before three minutes", started.Add(3*time.Minute - time.Nanosecond), true},
		{"exactly three minutes", started.Add(3 * time.Minute), false},
		{"after three minutes", started.Add(4 * time.Minute), false},
	}
	for _, tt := range tests {
		if got := d.clusterDriftStarting(tt.now); got != tt.want {
			t.Errorf("%s: clusterDriftStarting = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestClusterDriftStartingIsFalseWithNoRecordedStart: a daemon that recorded no
// start is not starting, so a zero start never holds the patrol back. The now
// is the zero time too, where the subtraction alone is inside the delay, so
// only the zero-start guard makes the answer false (gt-myyr6e).
func TestClusterDriftStartingIsFalseWithNoRecordedStart(t *testing.T) {
	t.Parallel()

	d := &Daemon{}
	if d.clusterDriftStarting(time.Time{}) {
		t.Fatal("clusterDriftStarting = true with no recorded start, want false")
	}
}
