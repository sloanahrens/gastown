package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
)

// sweepDaemonStart is the start of the daemon the probes judge records against.
func sweepDaemonStart() time.Time {
	return time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
}

// TestPodSweepLaunchAnswer pins the answer each outcome gives, and a record from
// an earlier daemon start: the started flag and the detail a person reads.
func TestPodSweepLaunchAnswer(t *testing.T) {
	t.Parallel()
	this := sweepDaemonStart()
	tests := []struct {
		name        string
		record      daemon.SweepSmokeRecord
		wantStarted bool
		wantDetail  string
	}{
		{"started", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "started"}, true, "launched"},
		{"busy", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "busy"}, true, "busy"},
		{"declined", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "declined"}, false, "declined"},
		{"failed", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "failed", Detail: "kubectl refused"}, false, "kubectl refused"},
		{"failed with no detail", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "failed"}, false, "the sweep launch failed"},
		{"stale record", daemon.SweepSmokeRecord{DaemonStart: this.Add(-time.Hour), Outcome: "started"}, false, "before this daemon's start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			started, detail, err := sweepLaunchAnswer(tt.record, this)
			if err != nil || started != tt.wantStarted || !strings.Contains(detail, tt.wantDetail) {
				t.Errorf("sweepLaunchAnswer = (%v, %q, %v), want started %v with detail containing %q and no error",
					started, detail, err, tt.wantStarted, tt.wantDetail)
			}
		})
	}
}

// TestPodSweepLaunchAnswerRefuses pins the error: an outcome the record does not
// name is not a verdict.
func TestPodSweepLaunchAnswerRefuses(t *testing.T) {
	t.Parallel()
	started, detail, err := sweepLaunchAnswer(daemon.SweepSmokeRecord{DaemonStart: sweepDaemonStart(), Outcome: "unknown"}, sweepDaemonStart())
	if err == nil {
		t.Errorf("sweepLaunchAnswer = (%v, %q, nil), want an error naming no known outcome", started, detail)
	}
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
