package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
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
	dead := poolSession{name: "dead/r/p", agent: "sandbox", rig: "r", polecat: "p", deadHooked: true, runsNothing: true}
	deadWorking := poolSession{name: "dead/r/p", agent: "sandbox", rig: "r", polecat: "p", deadHooked: true}
	live := poolSession{name: "r/p", agent: "sandbox", rig: "r", polecat: "p"}
	claim := poolSession{name: "pool-claim/c", agent: "sandbox", rig: "r", polecat: "p", claim: true}
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
		{"dead sandbox seat that runs nothing with no record is dropped", dead, sandbox, noRecord, false},
		{"dead sandbox seat that runs nothing with a record is kept", dead, sandbox, hasRecord, true},
		{"dead sandbox seat that runs nothing with a record read error is kept", dead, sandbox, errRecord, true},
		{"dead sandbox seat that does not run nothing is kept", deadWorking, sandbox, noRecord, true},
		{"live session is kept", live, sandbox, noRecord, true},
		{"claim is kept", claim, sandbox, noRecord, true},
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

func TestPoolSandboxlessRecordRead(t *testing.T) {
	t.Parallel()
	const rig, polecat = "gastown", "p0"
	match := session.SandboxRecord{Session: "gas-p0", Rig: rig, Polecat: polecat}
	record := func(s session.SandboxRecord) func(t *testing.T, town string) {
		return func(t *testing.T, town string) {
			if err := session.WriteSandboxRecord(town, s); err != nil {
				t.Fatalf("WriteSandboxRecord(%s): %v", s.Session, err)
			}
		}
	}
	file := func(path, body string) func(t *testing.T, town string) {
		return func(t *testing.T, town string) {
			p := filepath.Join(town, path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatalf("MkdirAll(%s): %v", filepath.Dir(p), err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatalf("WriteFile(%s): %v", path, err)
			}
		}
	}
	registry := func(name, body string) func(t *testing.T, town string) {
		return file(filepath.Join(".runtime", "sandboxes", name), body)
	}
	both := func(a, b func(t *testing.T, town string)) func(t *testing.T, town string) {
		return func(t *testing.T, town string) { a(t, town); b(t, town) }
	}
	tests := []struct {
		name    string
		setup   func(t *testing.T, town string)
		want    bool
		wantErr bool
	}{
		{"no registry directory", nil, false, false},
		{"no matching record", record(session.SandboxRecord{Session: "gas-p1", Rig: rig, Polecat: "p1"}), false, false},
		{"matching record, no rigs.json", record(match), true, false},
		{"matching record beside a corrupt one", both(record(match), registry("zz-corrupt.json", "{not json")), true, false},
		{"corrupt record, no match", registry("zz-corrupt.json", "{not json"), false, true},
		{"record with an empty polecat", record(session.SandboxRecord{Session: "gt-empty", Rig: rig}), false, true},
		{"record with an empty rig", record(session.SandboxRecord{Session: "gt-norig", Polecat: polecat}), false, true},
		{"listed name that is no session name", registry(".json", "{}"), false, true},
		{"registry path is a file", file(filepath.Join(".runtime", "sandboxes"), ""), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, town)
			}
			got, err := poolSandboxRecordRead(town)(rig, polecat)
			if (err != nil) != tt.wantErr {
				t.Fatalf("hasRecord(%q, %q) error = %v, wantErr %v", rig, polecat, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("hasRecord(%q, %q) = %v, want %v", rig, polecat, got, tt.want)
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
