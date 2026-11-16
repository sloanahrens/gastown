package done

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/role"
)

const intentTestAgentBead = "gt-gastown-polecat-nux"

// intentTestSeat is the polecat seat intentRecordRun's run ends on.
var intentTestSeat = intent.Seat{Rig: "gastown", Role: string(role.Polecat), Name: "nux"}

// intentRecordRun is a polecat run whose turn end both a test and gt done can
// read: a temporary town holds the intent record, and a fake bead store holds
// the agent bead the same facts mirror. No bd process starts and no tmux
// session is killed: the OpenShell backend is one gt done does not kill from
// this process.
func intentRecordRun(t *testing.T, bd beads.Client, warned *[]string) *doneRun {
	t.Helper()
	return &doneRun{
		issueID:        "gt-work",
		branch:         "polecat/nux/gt-work+abc",
		sender:         "gastown/polecats/nux",
		townRoot:       t.TempDir(),
		rigName:        "gastown",
		polecatName:    "nux",
		agentBeadID:    intentTestAgentBead,
		cleanupStatus:  "clean",
		agent:          Agent{Role: role.Polecat, Rig: "gastown", Polecat: "nux"},
		sessionBackend: backendOpenShell,
		env:            func(string) string { return "" },
		logEvent:       func(string, string, map[string]interface{}) error { return nil },
		openSource:     func(string, string) beads.Client { return bd },
		agentBD:        bd,
		warn: func(format string, args ...any) {
			*warned = append(*warned, fmt.Sprintf(format, args...))
		},
	}
}

// seededAgentBead is the agent bead a run's completion metadata is written in.
func seededAgentBead() beads.Issue {
	return beads.Issue{
		ID:     intentTestAgentBead,
		Title:  "Polecat nux",
		Status: "open",
		Labels: []string{"gt:agent"},
		Description: beads.FormatAgentDescription("Polecat nux", &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: "working", HookBead: "gt-work",
		}),
	}
}

// recordProbe is the agent-bead store a run writes: it reads the seat's intent
// record in the same instant each agent-bead write arrives, so a test can see
// what the record held when the bead mirror was written.
type recordProbe struct {
	beads.Client
	town     string
	seat     intent.Seat
	agentID  string
	writes   int
	atWrite  intent.Record
	writeErr error
}

func (p *recordProbe) Update(id string, opts beads.UpdateOptions) error {
	if id == p.agentID {
		p.writes++
		p.atWrite, p.writeErr = intent.Read(p.town, p.seat)
	}
	return p.Client.Update(id, opts)
}

// A gt done exit leaves the turn's end on the seat's intent record: the state
// it ended as, its exit type, the cleanup status the session reported and the
// bead it was working on. Before this bead the record could say a seat was
// still running a turn that gt done had already ended (gt-ijldj7).
func TestAgentBeadRetireTurnEndReachesTheIntentRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		exit      string
		turnState string
	}{
		{exit: ExitCompleted, turnState: string(beads.AgentStateDone)},
		{exit: ExitDeferred, turnState: string(beads.AgentStateStuck)},
		{exit: ExitEscalated, turnState: string(beads.AgentStateStuck)},
	}
	for _, tt := range tests {
		t.Run(tt.exit, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New(beadsfake.WithPrefix("gt"))
			db.Seed(seededAgentBead())
			var warned []string
			r := intentRecordRun(t, db, &warned)

			if err := reportDone(r, tt.exit); err != nil {
				t.Fatalf("reportDone(%s): %v", tt.exit, err)
			}

			rec, err := intent.Read(r.townRoot, intentTestSeat)
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
			if rec.LastWorkBead != "gt-work" {
				t.Errorf("last_work_bead = %q, want gt-work", rec.LastWorkBead)
			}
			if len(warned) != 0 {
				t.Errorf("a recorded turn end warned: %v", warned)
			}
		})
	}
}

// The record is written before the agent bead that mirrors it: a supervisor
// reading the two must never find the mirror ahead of the record (gt-ijldj7).
func TestAgentBeadRetireTurnEndPrecedesTheAgentBead(t *testing.T) {
	t.Parallel()
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(seededAgentBead())
	probe := &recordProbe{Client: db, seat: intentTestSeat, agentID: intentTestAgentBead}
	var warned []string
	r := intentRecordRun(t, probe, &warned)
	probe.town = r.townRoot

	if err := reportDone(r, ExitCompleted); err != nil {
		t.Fatalf("reportDone: %v", err)
	}

	if probe.writes != 1 {
		t.Fatalf("agent bead written %d times, want 1", probe.writes)
	}
	if probe.writeErr != nil {
		t.Fatalf("read the intent record at the bead write: %v", probe.writeErr)
	}
	if probe.atWrite.TurnState != string(beads.AgentStateDone) || probe.atWrite.LastWorkBead != "gt-work" {
		t.Errorf("intent record at the agent-bead write = %+v, want the turn's end already recorded", probe.atWrite)
	}
}

// An intent record that cannot be written is reported, and the agent-bead
// write the run still owes happens (gt-ijldj7).
func TestAgentBeadRetireFailedTurnEndWriteIsReported(t *testing.T) {
	t.Parallel()
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(seededAgentBead())
	var warned []string
	r := intentRecordRun(t, db, &warned)
	// A regular file where the intent lock directory belongs fails the record
	// write at once, without depending on file modes.
	if err := os.MkdirAll(filepath.Join(r.townRoot, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.townRoot, ".runtime", "locks"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := reportDone(r, ExitCompleted); err != nil {
		t.Fatalf("reportDone over a failed intent write = %v, want nil", err)
	}

	want := "couldn't record the turn's end in the intent record of " + intentTestSeat.String()
	if !strings.Contains(strings.Join(warned, "\n"), want) {
		t.Errorf("warnings lack %q:\n%s", want, strings.Join(warned, "\n"))
	}
	agent, err := db.Show(intentTestAgentBead)
	if err != nil {
		t.Fatalf("show the agent bead: %v", err)
	}
	if got := beads.ParseAgentFields(agent.Description).ExitType; got != ExitCompleted {
		t.Errorf("agent bead exit_type = %q, want %q: the bead write after the failed record write did not happen", got, ExitCompleted)
	}
}

// A crew run has no seat: gt done submits its branch and retires no session, so
// the turn-end write must name none rather than write a polecat seat under a
// crew member's name (gt-ijldj7).
func TestAgentBeadRetireTurnEndNamesNoSeatForCrew(t *testing.T) {
	t.Parallel()
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(seededAgentBead())
	var warned []string
	r := intentRecordRun(t, db, &warned)
	r.agent = Agent{Role: role.Crew, Rig: "gastown", Polecat: "nux"}

	if err := reportDone(r, ExitCompleted); err != nil {
		t.Fatalf("reportDone: %v", err)
	}

	if _, err := os.Stat(filepath.Join(r.townRoot, ".runtime", "agents")); !os.IsNotExist(err) {
		t.Fatalf("a crew run wrote an intent record (stat err %v); crew have no seat", err)
	}
}

// The cleanup self-report reaches the seat's intent record even when there is
// no agent bead to mirror it: the record needs the seat, not a Dolt row.
func TestAgentBeadRetireCleanupStatusLandsWithoutAnAgentBead(t *testing.T) {
	t.Parallel()
	r := cleanupStatusRun(t)
	var warned []string
	r.warn = func(format string, args ...any) {
		warned = append(warned, fmt.Sprintf(format, args...))
	}
	updater := &doneFakeCleanupUpdater{}

	r.selfReportCleanupStatus(unobservableGit{}, updater, "", "clean")

	seat := intent.Seat{Rig: r.rigName, Role: string(role.Polecat), Name: r.polecatName}
	rec, err := intent.Read(r.townRoot, seat)
	if err != nil {
		t.Fatalf("read the intent record: %v", err)
	}
	if rec.CleanupStatus != "clean" {
		t.Errorf("cleanup_status = %q, want clean", rec.CleanupStatus)
	}
	if updater.calls != 0 {
		t.Errorf("UpdateAgentCleanupStatus calls = %d, want 0 with no agent bead ID", updater.calls)
	}
	if !strings.Contains(strings.Join(warned, "\n"), "no agent bead ID") {
		t.Errorf("warnings lack the missing-agent-bead notice:\n%s", strings.Join(warned, "\n"))
	}
}

// A run that cannot name its seat refuses the record write and says so. The
// rig and polecat are known here and only the town root is missing, which is
// the case that would otherwise write the record at a path relative to the
// process's own directory, where no supervisor reads it (gt-ijldj7).
func TestAgentBeadRetireTurnEndRefusesASeatItCannotName(t *testing.T) {
	t.Parallel()
	var warned []string
	r := &doneRun{
		townRoot:    "",
		rigName:     "gastown",
		polecatName: "nux",
		agent:       Agent{Role: role.Polecat, Rig: "gastown", Polecat: "nux"},
		warn: func(format string, args ...any) {
			warned = append(warned, fmt.Sprintf(format, args...))
		},
	}

	r.recordTurnEndInIntent(intent.TurnEnd{TurnState: string(beads.AgentStateDone), ExitType: ExitCompleted})

	if !strings.Contains(strings.Join(warned, "\n"), "no seat to name") {
		t.Errorf("warnings lack the unnamed-seat refusal:\n%s", strings.Join(warned, "\n"))
	}
}
