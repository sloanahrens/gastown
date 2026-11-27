package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
)

// sweepDaemonStart is the start of the daemon the probes judge records against.
func sweepDaemonStart() time.Time {
	return time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
}

// TestPodSweepLaunchStart pins the daemon start the record is judged against:
// a town whose state names no start is an error, since a zero start would judge
// every record current.
func TestPodSweepLaunchStart(t *testing.T) {
	t.Parallel()
	town := func() (string, error) { return "/town", nil }
	tests := []struct {
		name    string
		find    func() (string, error)
		state   *daemon.State
		loadErr error
		wantErr bool
	}{
		{"a state with a start is read", town, &daemon.State{StartedAt: sweepDaemonStart()}, nil, false},
		{"a state with no start is an error", town, &daemon.State{}, nil, true},
		{"a town that is not found is an error", func() (string, error) { return "", errors.New("no town") }, nil, nil, true},
		{"a state that cannot be read is an error", town, nil, errors.New("unreadable"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			load := func(string) (*daemon.State, error) { return tt.state, tt.loadErr }
			start, err := daemonStartFrom(tt.find, load)
			if (err != nil) != tt.wantErr || (!tt.wantErr && !start.Equal(sweepDaemonStart())) {
				t.Errorf("daemonStartFrom = (%v, %v), want the start %v and error %v", start, err, sweepDaemonStart(), tt.wantErr)
			}
		})
	}
}
