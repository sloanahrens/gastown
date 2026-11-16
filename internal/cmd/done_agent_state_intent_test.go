package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/intent"
)

// doneIntentSeat is the polecat seat the agent-state path ends a turn on.
var doneIntentSeat = intent.Seat{Rig: "gastown", Role: "polecat", Name: "nux"}

// doneIntentEnv is the environment the agent-state write reads in the tests
// here: a polecat identity, fake bead stores, and a warning seam so a test can
// read what gt done told the operator.
func doneIntentEnv(t *testing.T, db beads.Client, warned *[]string) doneStateEnv {
	t.Helper()
	return doneStateEnv{
		getenv:     envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed:     func(string) beads.Client { return db },
		source:     func(string, string) beads.Client { return db },
		purge:      func(string, string) {},
		reviewHead: func(string) (string, error) { return "", nil },
		warn: func(format string, args ...any) {
			*warned = append(*warned, fmt.Sprintf(format, args...))
		},
	}
}

// pendingForExit is what reportDone hands the state write for an exit of that
// type: the completion metadata and cleanup_status.
func pendingForExit(exit string) beads.AgentFieldUpdates {
	pending := beads.CompletionUpdates(&beads.CompletionMetadata{
		ExitType:       exit,
		Branch:         "polecat/nux/gt-base-123+abc",
		HookBead:       "gt-base-123",
		CompletionTime: "2026-10-13T12:00:00Z",
	})
	clean := "clean"
	pending.CleanupStatus = &clean
	return pending
}

// doneIntentTown seeds a town with the agent bead, the hooked work bead, and a
// fake store so gt done's state write runs without bd.
func doneIntentTown(t *testing.T) (string, *beadsfake.Fake) {
	t.Helper()
	townRoot := doneBatchTown(t)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked)})
	return townRoot, db
}

// A gt done exit leaves the turn's end on the polecat's intent record: the
// state it ended as, its exit type, the cleanup status gt done recorded and the
// work bead it was on. The agent bead's copy must say the same (gt-ijldj7).
func TestAgentBeadRetireTurnEndReachesTheIntentRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		exit      string
		turnState string
	}{
		{exit: done.ExitCompleted, turnState: string(beads.AgentStateDone)},
		{exit: done.ExitDeferred, turnState: string(beads.AgentStateStuck)},
		{exit: done.ExitEscalated, turnState: string(beads.AgentStateStuck)},
	}
	for _, tt := range tests {
		t.Run(tt.exit, func(t *testing.T) {
			t.Parallel()
			townRoot, db := doneIntentTown(t)
			var warned []string
			env := doneIntentEnv(t, db, &warned)

			if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, tt.exit, "gt-base-123", pendingForExit(tt.exit)); err != nil {
				t.Fatalf("gt done: %v", err)
			}

			rec, err := intent.Read(townRoot, doneIntentSeat)
			if err != nil {
				t.Fatalf("read the intent record: %v", err)
			}
			if rec.TurnState != tt.turnState {
				t.Errorf("turn_state = %q, want %q", rec.TurnState, tt.turnState)
			}
			if rec.ExitType != tt.exit {
				t.Errorf("exit_type = %q, want %q", rec.ExitType, tt.exit)
			}
			if rec.CleanupStatus != "clean" {
				t.Errorf("cleanup_status = %q, want clean", rec.CleanupStatus)
			}
			if rec.LastWorkBead != "gt-base-123" {
				t.Errorf("last_work_bead = %q, want gt-base-123", rec.LastWorkBead)
			}
			agent, err := db.Show("gt-gastown-polecat-nux")
			if err != nil {
				t.Fatalf("show the agent bead: %v", err)
			}
			if got := beads.ParseAgentFields(agent.Description).ExitType; got != rec.ExitType {
				t.Errorf("agent bead exit_type = %q, the record says %q: the mirror and the record drifted", got, rec.ExitType)
			}
			if len(warned) != 0 {
				t.Errorf("a recorded turn end warned: %v", warned)
			}
		})
	}
}

// recordProbe is the agent-bead store the state write uses: it reads the seat's
// intent record in the same instant each agent-bead write arrives, so a test
// can see what the record held when the bead mirror was written.
type recordProbe struct {
	beads.Client
	town    string
	seat    intent.Seat
	agentID string
	writes  int
	atWrite intent.Record
	readErr error
}

func (p *recordProbe) Update(id string, opts beads.UpdateOptions) error {
	if id == p.agentID {
		p.writes++
		p.atWrite, p.readErr = intent.Read(p.town, p.seat)
	}
	return p.Client.Update(id, opts)
}

// The record is written before the agent bead that mirrors it, so a reader
// never sees the mirror ahead of the record (gt-ijldj7).
func TestAgentBeadRetireTurnEndPrecedesTheAgentBead(t *testing.T) {
	t.Parallel()
	townRoot, db := doneIntentTown(t)
	probe := &recordProbe{Client: db, town: townRoot, seat: doneIntentSeat, agentID: "gt-gastown-polecat-nux"}
	var warned []string
	env := doneIntentEnv(t, probe, &warned)

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingForExit(done.ExitCompleted)); err != nil {
		t.Fatalf("gt done: %v", err)
	}

	if probe.writes != 1 {
		t.Fatalf("agent bead written %d times, want 1", probe.writes)
	}
	if probe.readErr != nil {
		t.Fatalf("read the intent record at the bead write: %v", probe.readErr)
	}
	if probe.atWrite.TurnState != string(beads.AgentStateDone) || probe.atWrite.LastWorkBead != "gt-base-123" {
		t.Errorf("intent record at the agent-bead write = %+v, want the turn's end already recorded", probe.atWrite)
	}
}

// An intent record that cannot be written is reported, and the agent-bead
// write the same facts mirror still happens (gt-ijldj7).
func TestAgentBeadRetireFailedTurnEndWriteIsReported(t *testing.T) {
	t.Parallel()
	townRoot, db := doneIntentTown(t)
	// A regular file where the intent lock directory belongs fails the record
	// write at once, without depending on file modes.
	if err := os.MkdirAll(filepath.Join(townRoot, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime", "locks"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warned []string
	env := doneIntentEnv(t, db, &warned)

	consumed, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingForExit(done.ExitCompleted))
	if err != nil {
		t.Fatalf("gt done over a failed intent write = %v, want nil", err)
	}
	if !consumed {
		t.Error("consumed = false: the caller would write the completion metadata a second time")
	}

	want := "couldn't record the turn's end in the intent record of " + doneIntentSeat.String()
	if !strings.Contains(strings.Join(warned, "\n"), want) {
		t.Errorf("warnings lack %q:\n%s", want, strings.Join(warned, "\n"))
	}
	agent, err := db.Show("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("show the agent bead: %v", err)
	}
	if got := beads.ParseAgentFields(agent.Description).AgentState; got != string(beads.AgentStateDone) {
		t.Errorf("agent_state = %q, want done: the bead write after the failed record write did not happen", got)
	}
}

// A crew run has no seat: its gt done submits a branch and retires no session,
// so the turn-end write must name no seat rather than write a polecat record
// under a crew member's name (gt-ijldj7).
func TestAgentBeadRetireTurnEndNamesNoSeatForCrew(t *testing.T) {
	t.Parallel()
	townRoot := doneBatchTown(t)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{
		ID:          "gt-gastown-crew-bob",
		Title:       "Crew bob",
		Status:      "open",
		Labels:      []string{"gt:agent"},
		Description: beads.FormatAgentDescription("Crew bob", &beads.AgentFields{RoleType: "crew", Rig: "gastown", AgentState: "working", HookBead: "gt-base-123"}),
	}, beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked)})
	var warned []string
	env := doneIntentEnv(t, db, &warned)
	env.getenv = envMap(map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "bob"})

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingForExit(done.ExitCompleted)); err != nil {
		t.Fatalf("gt done: %v", err)
	}

	if _, err := os.Stat(filepath.Join(townRoot, ".runtime", "agents")); !os.IsNotExist(err) {
		t.Fatalf("a crew run wrote an intent record (stat err %v); crew have no seat", err)
	}
}
