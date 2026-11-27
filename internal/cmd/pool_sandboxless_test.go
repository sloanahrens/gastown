package cmd

import (
	"errors"
	"testing"
)

func TestPoolSandboxlessRunsNothing(t *testing.T) {
	t.Parallel()
	readErr := errors.New("beads down")
	tests := []struct {
		name  string
		state poolSeatState
		err   error
		want  bool
	}{
		{"measured unheld idle seat", poolSeatState{DeadRead: true}, nil, true},
		{"unmeasured seat", poolSeatState{}, nil, false},
		{"held work", poolSeatState{DeadRead: true, HookHeld: true}, nil, false},
		{"working agent bead", poolSeatState{DeadRead: true, AgentWorking: true}, nil, false},
		{"read error", poolSeatState{DeadRead: true}, readErr, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := seatRunsNothing(tt.state, tt.err); got != tt.want {
				t.Errorf("seatRunsNothing(%+v, %v) = %v, want %v", tt.state, tt.err, got, tt.want)
			}
		})
	}
}
