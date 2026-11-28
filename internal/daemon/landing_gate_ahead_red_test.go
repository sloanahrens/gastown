package daemon

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/stretchr/testify/require"
)

// TestGateAheadMainRed pins the gate-ahead question to the rig's main state: a
// green record is clean, a run past the last green or a revert in flight is red,
// and a state or revert bead that cannot be read is red with one log line each.
func TestGateAheadMainRed(t *testing.T) {
	t.Parallel()
	green, next := "0123456789abcdef0123456789abcdef01234567", "fedcba9876543210fedcba9876543210fedcba98"
	revert := landworker.MainState{LastGreen: green, LastRun: green, Revert: &landworker.PendingRevert{Culprit: next, Bead: "gt-rev"}}
	tests := []struct {
		name     string
		state    landworker.MainState
		stateErr error
		open     func(string, string) (bool, error)
		want     bool
		wantLog  string
	}{
		{"a green record is clean", landworker.MainState{LastGreen: green, LastRun: green}, nil, nil, false, ""},
		{"a last run past the last green is red", landworker.MainState{LastGreen: green, LastRun: next}, nil, nil, true, ""},
		{"a revert in flight is red", revert, nil,
			func(string, string) (bool, error) { return true, nil }, true, ""},
		{"a closed revert bead is clean", revert, nil,
			func(string, string) (bool, error) { return false, nil }, false, ""},
		{"an unreadable state is red", landworker.MainState{}, errors.New("reading main state"), nil, true,
			"gate ahead: gastown: main state unreadable, counting as red: reading main state"},
		{"an unreadable revert bead is red", revert, nil,
			func(string, string) (bool, error) { return false, errors.New("bd: timed out") }, true,
			"gate ahead: gastown: revert bead unreadable, counting as red: bd: timed out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs []string
			state := func(string) (landworker.MainState, error) { return tt.state, tt.stateErr }
			logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			check := gateAheadRedCheck("gastown", state, tt.open, logf)
			require.NotNil(t, check)
			require.Equal(t, tt.want, check())
			if tt.wantLog == "" {
				require.Empty(t, logs, "a readable main logs nothing")
				return
			}
			require.Equal(t, []string{tt.wantLog}, logs, "an unreadable record logs one line naming the rig and the error")
		})
	}
}

// TestGateAheadMainRedSwitchOff pins the switch off: with no state loader there is
// no question for the landing worker to ask.
func TestGateAheadMainRedSwitchOff(t *testing.T) {
	t.Parallel()
	require.Nil(t, gateAheadRedCheck("gastown", nil, nil, func(string, ...any) {}))
}
