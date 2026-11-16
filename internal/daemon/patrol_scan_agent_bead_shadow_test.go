package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// agentBeadRetireHost is a host over a temp town holding one agent bead in the
// fake beads database, with the daemon's log captured. The shadow lines are
// remembered for the whole daemon process, so the seat's entries are dropped
// before and after the test: a line this test's seat logged in another test
// must not stand in for one it should write here.
func agentBeadRetireHost(t *testing.T, rig, name string, fields beads.AgentFields) (*patrolScanHost, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:          beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, rig), rig, name),
		Description: beads.FormatAgentDescription(name, &fields),
	})
	logs := &bytes.Buffer{}
	h := &patrolScanHost{d: &Daemon{
		config:        &Config{TownRoot: town},
		openWorkBeads: bd.open,
		logger:        log.New(logs, "", 0),
	}}
	seat := rig + "/" + name
	forgetAgentBeadShadow(seat)
	t.Cleanup(func() { forgetAgentBeadShadow(seat) })
	return h, logs
}

// forgetAgentBeadShadow drops the lines one seat has logged in this process,
// leaving other seats' entries to their own tests.
func forgetAgentBeadShadow(seat string) {
	agentBeadShadowMu.Lock()
	defer agentBeadShadowMu.Unlock()
	for key := range agentBeadShadowLogged {
		if key.seat == seat {
			delete(agentBeadShadowLogged, key)
		}
	}
}

// writeAgentBeadRetireRecord writes the seat's intent record.
func writeAgentBeadRetireRecord(t *testing.T, h *patrolScanHost, rig, name string, rec intent.Record) {
	t.Helper()
	writeJSONFile(t, intent.Seat{Rig: rig, Role: constants.RolePolecat, Name: name}.Path(h.town()), rec)
}

// TestAgentBeadRetireLogsEachDisagreementOnce holds the shadow log to one line
// per seat, field and value pair: a second read of the same disagreement
// writes nothing, and a record whose value changes writes again. Without the
// remembered set the window would log the same drift on every tick and drown
// the change the reader is looking for.
func TestAgentBeadRetireLogsEachDisagreementOnce(t *testing.T) {
	t.Parallel()
	const rig, name = "retire", "shadow-once"
	h, logs := agentBeadRetireHost(t, rig, name, beads.AgentFields{
		RoleType:   constants.RolePolecat,
		Rig:        rig,
		AgentState: "stuck",
		ExitType:   "DEFERRED",
	})
	read := func() {
		t.Helper()
		if _, err := h.AgentRecord(rig, name); err != nil {
			t.Fatalf("AgentRecord: %v", err)
		}
	}
	// The record agrees on every other fact, so exit_type is the one
	// disagreement these reads find.
	writeAgentBeadRetireRecord(t, h, rig, name, intent.Record{TurnState: "stuck", ExitType: "COMPLETED"})

	read()
	want := "agentbead_shadow: retire/shadow-once exit_type intent=COMPLETED bead=DEFERRED\n"
	if got := logs.String(); got != want {
		t.Fatalf("first read logged %q, want %q", got, want)
	}

	read()
	if got := logs.String(); got != want {
		t.Fatalf("a repeated read logged again: %q, want %q", got, want)
	}

	writeAgentBeadRetireRecord(t, h, rig, name, intent.Record{TurnState: "stuck", ExitType: "ESCALATED"})
	read()
	want += "agentbead_shadow: retire/shadow-once exit_type intent=ESCALATED bead=DEFERRED\n"
	if got := logs.String(); got != want {
		t.Fatalf("a changed value logged %q, want %q", got, want)
	}
}

// TestAgentBeadRetireReturnsTheBeadValues holds AgentRecord to the bead's
// values whether the intent record agrees with it or not: while the shadow
// window runs, the bead is the source and the record only what is compared
// against it.
func TestAgentBeadRetireReturnsTheBeadValues(t *testing.T) {
	t.Parallel()
	const rig = "retire"
	fields := beads.AgentFields{
		RoleType:        constants.RolePolecat,
		Rig:             rig,
		AgentState:      "stuck",
		ExitType:        "DEFERRED",
		CleanupStatus:   "has_unpushed",
		HookBead:        "gt-hook",
		LastSourceIssue: "gt-last",
	}
	want := patrolscan.AgentRecord{
		State:           "stuck",
		ExitType:        "DEFERRED",
		CleanupStatus:   "has_unpushed",
		HookBead:        "gt-hook",
		LastSourceIssue: "gt-last",
	}
	tests := []struct {
		name   string
		record intent.Record
	}{
		{
			name: "a record that agrees",
			record: intent.Record{TurnState: "stuck", ExitType: "DEFERRED", CleanupStatus: "has_unpushed",
				WorkBead: "gt-hook", LastWorkBead: "gt-last"},
		},
		{
			name: "a record that disagrees",
			record: intent.Record{TurnState: "done", ExitType: "COMPLETED", CleanupStatus: "clean",
				WorkBead: "gt-other", LastWorkBead: "gt-earlier"},
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := []string{"bead-return-agreeing", "bead-return-disagreeing"}[i]
			h, _ := agentBeadRetireHost(t, rig, name, fields)
			writeAgentBeadRetireRecord(t, h, rig, name, tc.record)
			got, err := h.AgentRecord(rig, name)
			if err != nil {
				t.Fatalf("AgentRecord: %v", err)
			}
			if got != want {
				t.Fatalf("AgentRecord = %+v, want the bead's %+v", got, want)
			}
		})
	}
}

// TestAgentBeadRetireUnreadableRecordLogsOnce holds the fail-closed read to the
// shadow's contract: a record that cannot be parsed is reported once, and the
// bead's record is still returned rather than an error the tick would read as
// no answer.
func TestAgentBeadRetireUnreadableRecordLogsOnce(t *testing.T) {
	t.Parallel()
	const rig, name = "retire", "shadow-broken"
	h, logs := agentBeadRetireHost(t, rig, name, beads.AgentFields{
		RoleType:        constants.RolePolecat,
		Rig:             rig,
		AgentState:      "stuck",
		ExitType:        "DEFERRED",
		CleanupStatus:   "has_unpushed",
		HookBead:        "gt-hook",
		LastSourceIssue: "gt-last",
	})
	path := intent.Seat{Rig: rig, Role: constants.RolePolecat, Name: name}.Path(h.town())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not a record"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := patrolscan.AgentRecord{
		State:           "stuck",
		ExitType:        "DEFERRED",
		CleanupStatus:   "has_unpushed",
		HookBead:        "gt-hook",
		LastSourceIssue: "gt-last",
	}
	for read := 1; read <= 2; read++ {
		got, err := h.AgentRecord(rig, name)
		if err != nil {
			t.Fatalf("read %d: AgentRecord: %v", read, err)
		}
		if got != want {
			t.Fatalf("read %d: AgentRecord = %+v, want the bead's %+v", read, got, want)
		}
	}
	if n := strings.Count(logs.String(), agentBeadShadowLogPrefix); n != 1 {
		t.Fatalf("logged %d lines, want 1: %q", n, logs.String())
	}
	if line := logs.String(); !strings.Contains(line, "agentbead_shadow: retire/shadow-broken intent record unreadable") {
		t.Fatalf("the line does not name the seat and the unreadable record: %q", line)
	}
}
