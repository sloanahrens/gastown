package cmd

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
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

func TestPoolSandboxlessFilter(t *testing.T) {
	t.Parallel()
	readErr := errors.New("beads down")
	landing := poolSession{name: "landing/r/p", agent: "sandbox", rig: "r", polecat: "p", landing: true}
	// keeper is a host landing seat, so a drop must leave it untouched.
	keeper := poolSession{name: "landing/r/host", agent: "host", rig: "r", polecat: "host", landing: true}
	sandbox := func(a string) bool { return a == "sandbox" }
	noRecord := func(string, string) (bool, error) { return false, nil }
	hasRecord := func(string, string) (bool, error) { return true, nil }
	errRecord := func(string, string) (bool, error) { return false, readErr }
	tests := []struct {
		name string
		seat poolSession
		is   func(string) bool
		rec  func(string, string) (bool, error)
		keep bool
	}{
		{"landing sandbox seat with no record is dropped", landing, sandbox, noRecord, false},
		{"landing sandbox seat with a record is kept", landing, sandbox, hasRecord, true},
		{"record read error keeps the seat", landing, sandbox, errRecord, true},
		{"host agent keeps the seat", landing, func(string) bool { return false }, noRecord, true},
		{"non-landing seat is kept", poolSession{name: "r", agent: "sandbox", rig: "r", polecat: "p"}, sandbox, noRecord, true},
		{"seat naming no rig or polecat is kept", poolSession{name: "landing//", agent: "sandbox", landing: true}, sandbox, noRecord, true},
		{"nil isSandboxAgent returns every seat", landing, nil, noRecord, true},
		{"nil hasRecord returns every seat", landing, sandbox, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := []poolSession{tt.seat, keeper}
			before := append([]poolSession(nil), in...)
			got := withoutSandboxlessSeats(in, tt.is, tt.rec)
			want := in
			if !tt.keep {
				want = []poolSession{keeper}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(in, before) {
				t.Errorf("input modified: %+v, was %+v", in, before)
			}
		})
	}
}

func TestPoolSandboxlessAgentRead(t *testing.T) {
	t.Parallel()
	const town = `{"type":"town-settings","version":1,"sandboxes":{"p":{"image":"i","policy":"p"}},"polecat_pool":{"seats":{"sandbox":{"cap":1,"backend":"openshell","sandbox":"p"},"host":{"cap":1}}}}`
	cases := []struct {
		name     string
		body     string
		agents   []string
		want     []bool
		warnings int
	}{
		{"openshell seat", town, []string{"sandbox"}, []bool{true}, 0},
		{"host seat and unknown agent", town, []string{"host", "nobody"}, []bool{false, false}, 0},
		{"unreadable town settings", "{not json", []string{"sandbox", "host"}, []bool{false, false}, 1},
		{"unreadable seats", `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"sandbox":{"cap":1,"backend":"openshell"}}}}`, []string{"sandbox"}, []bool{false}, 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root, _ := writeProfileTown(t, tt.body)
			var warn bytes.Buffer
			isSandbox := poolSandboxAgentRead(root, &warn)
			for i, agent := range tt.agents {
				if got := isSandbox(agent); got != tt.want[i] {
					t.Errorf("isSandboxAgent(%q) = %v, want %v", agent, got, tt.want[i])
				}
			}
			if got := strings.Count(warn.String(), "warning:"); got != tt.warnings {
				t.Errorf("warnings = %d (%q), want %d", got, warn.String(), tt.warnings)
			}
		})
	}
}
