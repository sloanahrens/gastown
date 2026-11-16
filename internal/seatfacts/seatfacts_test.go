package seatfacts

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// retireSeat is the seat these tests read and write: a rig-level polecat.
var retireSeat = intent.Seat{Rig: "gastown", Role: "polecat", Name: "ruby"}

// retireNow fixes the time the records are written at, so nothing depends on
// the wall clock.
var retireNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// TestAgentBeadRetireRead holds Read to the facts its intent record carries,
// to the zero facts of a seat with no record, and to its fail-closed answer
// for a record it cannot read.
func TestAgentBeadRetireRead(t *testing.T) {
	t.Parallel()
	end := intent.TurnEnd{
		TurnState:     "stuck",
		ExitType:      "DEFERRED",
		CleanupStatus: "has_unpushed",
		WorkBead:      "gt-last",
	}
	tests := []struct {
		name    string
		write   func(t *testing.T, townRoot string)
		want    Facts
		wantErr bool
	}{
		{
			name:  "a seat with no record",
			write: func(*testing.T, string) {},
			want:  Facts{Desired: intent.DesiredRun},
		},
		{
			name: "a turn-ended record",
			write: func(t *testing.T, townRoot string) {
				if err := intent.MarkTurnEnded(townRoot, retireSeat, end, "gt done", retireNow); err != nil {
					t.Fatal(err)
				}
			},
			want: Facts{
				TurnState:     "stuck",
				ExitType:      "DEFERRED",
				CleanupStatus: "has_unpushed",
				LastWorkBead:  "gt-last",
				Desired:       intent.DesiredRun,
			},
		},
		{
			name: "a submitted record holding its work bead",
			write: func(t *testing.T, townRoot string) {
				if err := intent.MarkSubmitted(townRoot, retireSeat, "gt-work", "gt done", retireNow); err != nil {
					t.Fatal(err)
				}
			},
			want: Facts{WorkBead: "gt-work", Desired: intent.DesiredSubmitted},
		},
		{
			name: "a parked record",
			write: func(t *testing.T, townRoot string) {
				_, err := intent.Update(townRoot, retireSeat, func(r *intent.Record) error {
					r.Desired = intent.DesiredPark
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			},
			want: Facts{Desired: intent.DesiredPark, Held: true},
		},
		{
			name: "a malformed record",
			write: func(t *testing.T, townRoot string) {
				writeRawRecord(t, townRoot, `{"turn_state": "done"`)
			},
			want:    heldFacts(),
			wantErr: true,
		},
		{
			name: "a record path that is a directory",
			write: func(t *testing.T, townRoot string) {
				if err := os.MkdirAll(retireSeat.Path(townRoot), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want:    heldFacts(),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			tc.write(t, townRoot)

			got, err := Read(townRoot, retireSeat)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Read = %+v, nil error; want the held facts and an error", got)
				}
				if got != tc.want {
					t.Errorf("Read = %+v, want %+v", got, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got != tc.want {
				t.Errorf("Read = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// heldFacts is what Read returns for a record it cannot read: a caller that
// drops the error still may not read the seat as free.
func heldFacts() Facts {
	return Facts{Desired: intent.DesiredPark, Held: true}
}

// writeRawRecord puts body at the seat's record path, so a test can write the
// record Read must refuse. The intent writers only ever write valid JSON.
func writeRawRecord(t *testing.T, townRoot, body string) {
	t.Helper()
	path := retireSeat.Path(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAgentBeadRetireDisagreements holds Disagreements to one entry per
// differing field, in the order the fields are compared, and to nothing for
// equal records — empty against empty included.
func TestAgentBeadRetireDisagreements(t *testing.T) {
	t.Parallel()
	equalFacts := Facts{
		TurnState:     "done",
		ExitType:      "COMPLETED",
		CleanupStatus: "clean",
		WorkBead:      "gt-work",
		LastWorkBead:  "gt-last",
		Desired:       intent.DesiredSubmitted,
	}
	equalBead := patrolscan.AgentRecord{
		State:           "done",
		ExitType:        "COMPLETED",
		CleanupStatus:   "clean",
		HookBead:        "gt-work",
		LastSourceIssue: "gt-last",
	}
	tests := []struct {
		name string
		f    Facts
		a    patrolscan.AgentRecord
		want []Disagreement
	}{
		{
			name: "equal records",
			f:    equalFacts,
			a:    equalBead,
		},
		{
			name: "empty against empty",
			f:    Facts{},
			a:    patrolscan.AgentRecord{},
		},
		{
			name: "a different exit type",
			f:    withFacts(equalFacts, func(f *Facts) { f.ExitType = "DEFERRED" }),
			a:    equalBead,
			want: []Disagreement{{Field: "exit_type", Intent: "DEFERRED", Bead: "COMPLETED"}},
		},
		{
			name: "a different cleanup status on the bead",
			f:    equalFacts,
			a:    withBead(equalBead, func(a *patrolscan.AgentRecord) { a.CleanupStatus = "has_uncommitted" }),
			want: []Disagreement{{Field: "cleanup_status", Intent: "clean", Bead: "has_uncommitted"}},
		},
		{
			name: "a different work bead",
			f:    withFacts(equalFacts, func(f *Facts) { f.WorkBead = "" }),
			a:    equalBead,
			want: []Disagreement{{Field: "work_bead", Intent: "", Bead: "gt-work"}},
		},
		{
			name: "a different last work bead on the bead",
			f:    equalFacts,
			a:    withBead(equalBead, func(a *patrolscan.AgentRecord) { a.LastSourceIssue = "gt-other" }),
			want: []Disagreement{{Field: "last_work_bead", Intent: "gt-last", Bead: "gt-other"}},
		},
		{
			name: "a different turn state",
			f:    withFacts(equalFacts, func(f *Facts) { f.TurnState = "stuck" }),
			a:    equalBead,
			want: []Disagreement{{Field: "turn_state", Intent: "stuck", Bead: "done"}},
		},
		{
			name: "every field differs",
			f: Facts{
				TurnState:     "awaiting-gate",
				ExitType:      "ESCALATED",
				CleanupStatus: "has_stash",
				WorkBead:      "",
				LastWorkBead:  "",
			},
			a: patrolscan.AgentRecord{
				State:           "done",
				ExitType:        "COMPLETED",
				CleanupStatus:   "clean",
				HookBead:        "gt-work",
				LastSourceIssue: "gt-last",
			},
			want: []Disagreement{
				{Field: "exit_type", Intent: "ESCALATED", Bead: "COMPLETED"},
				{Field: "cleanup_status", Intent: "has_stash", Bead: "clean"},
				{Field: "work_bead", Intent: "", Bead: "gt-work"},
				{Field: "last_work_bead", Intent: "", Bead: "gt-last"},
				{Field: "turn_state", Intent: "awaiting-gate", Bead: "done"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Disagreements(tc.f, tc.a); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Disagreements = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// withFacts returns a copy of f changed by change.
func withFacts(f Facts, change func(*Facts)) Facts {
	change(&f)
	return f
}

// withBead returns a copy of a changed by change.
func withBead(a patrolscan.AgentRecord, change func(*patrolscan.AgentRecord)) patrolscan.AgentRecord {
	change(&a)
	return a
}

// TestAgentBeadRetireTurnStateWindow holds the turn_state comparison to the
// states that make the two fields the same fact, and to the other states that
// leave it alone unless the record says a turn has ended.
func TestAgentBeadRetireTurnStateWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		f    Facts
		a    patrolscan.AgentRecord
		want []Disagreement
	}{
		{
			name: "a working bead with no turn recorded",
			a:    patrolscan.AgentRecord{State: "working"},
		},
		{
			name: "a paused bead with no turn recorded",
			a:    patrolscan.AgentRecord{State: "paused"},
		},
		{
			name: "a spawning bead with no turn recorded",
			a:    patrolscan.AgentRecord{State: "spawning"},
		},
		{
			name: "a done bead with no turn recorded",
			a:    patrolscan.AgentRecord{State: "done"},
			want: []Disagreement{{Field: "turn_state", Intent: "", Bead: "done"}},
		},
		{
			name: "a done bead that reads done in another case",
			f:    Facts{TurnState: "done"},
			a:    patrolscan.AgentRecord{State: "Done"},
			want: []Disagreement{{Field: "turn_state", Intent: "done", Bead: "Done"}},
		},
		{
			name: "a done bead the record agrees with",
			f:    Facts{TurnState: "done"},
			a:    patrolscan.AgentRecord{State: "done"},
		},
		{
			name: "a stuck bead the record disagrees with",
			f:    Facts{TurnState: "done"},
			a:    patrolscan.AgentRecord{State: "stuck"},
			want: []Disagreement{{Field: "turn_state", Intent: "done", Bead: "stuck"}},
		},
		{
			name: "an awaiting-gate bead the record agrees with",
			f:    Facts{TurnState: "awaiting-gate"},
			a:    patrolscan.AgentRecord{State: "awaiting-gate"},
		},
		{
			name: "a working bead the record says has ended a turn",
			f:    Facts{TurnState: "done"},
			a:    patrolscan.AgentRecord{State: "working"},
			want: []Disagreement{{Field: "turn_state", Intent: "done", Bead: "working"}},
		},
		{
			name: "a working bead whose value differs only in space",
			f:    Facts{TurnState: "done"},
			a:    patrolscan.AgentRecord{State: " done "},
			want: []Disagreement{{Field: "turn_state", Intent: "done", Bead: " done "}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Disagreements(tc.f, tc.a); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Disagreements = %+v, want %+v", got, tc.want)
			}
		})
	}
}
