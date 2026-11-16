package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
)

// seatShadowTown stages one sessionless polecat whose agent bead names hook as
// its hook, and returns the batch the dead-hooked walk reads it through with
// the window's lines captured, the town and the store.
func seatShadowTown(t *testing.T, name, hook string) (town string, db *beadsfake.Fake, batch *poolSeatBatch, lines *bytes.Buffer) {
	t.Helper()
	town = t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", name)
	db = beadsfake.New()
	seedPolecatAgentBead(db, town, "gastown", name, hook)
	lines = &bytes.Buffer{}
	batch = batchOverStore(town, db)
	batch.shadow = lines
	return town, db, batch, lines
}

// shadowLines is the batch's shadow output, one line per entry, and empty for
// a window that logged nothing.
func shadowLines(lines *bytes.Buffer) []string {
	out := strings.Split(strings.TrimSuffix(lines.String(), "\n"), "\n")
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// TestAgentBeadRetireDeadHookedLogsTheHookDisagreement is P1.7's line: a seat
// whose intent record names one work bead and whose agent bead names another is
// logged once, in the form the overseer greps, when the dead-hooked walk reads
// the seat. Without the compare in the walk the buffer stays empty.
func TestAgentBeadRetireDeadHookedLogsTheHookDisagreement(t *testing.T) {
	t.Parallel()
	town, _, batch, lines := seatShadowTown(t, "ember", "gt-bead-hook")
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ember"}, town,
		intent.Record{WorkBead: "gt-intent-bead"})

	if _, _, err := rosterOverShadowBatch(&fakeLister{}, town, batch); err != nil {
		t.Fatalf("roster: %v", err)
	}
	want := []string{"agentbead_shadow: gastown/ember hook intent=gt-intent-bead bead=gt-bead-hook"}
	if got := shadowLines(lines); !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow lines = %q, want %q", got, want)
	}
}

// TestAgentBeadRetireDeadHookedLogsOncePerSeat: the walk asks the seat's batch
// once, and a read that asks it again for the same seat does not log a second
// line — the window records a disagreeing seat, not each ask.
func TestAgentBeadRetireDeadHookedLogsOncePerSeat(t *testing.T) {
	t.Parallel()
	town, _, batch, lines := seatShadowTown(t, "ember", "gt-bead-hook")
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ember"}, town,
		intent.Record{WorkBead: "gt-intent-bead"})

	for range []int{0, 1} {
		if _, err := batch.hookHeld("gastown", "ember"); err != nil {
			t.Fatalf("hookHeld: %v", err)
		}
	}
	if got := shadowLines(lines); len(got) != 1 {
		t.Fatalf("shadow lines = %q, want one line for the one seat", got)
	}
}

// TestAgentBeadRetireDeadHookedCountIgnoresTheRecord: the count is the agent
// bead's either way, so a dead-hooked seat stays counted when its record names
// a different work bead, and the picture is the one the town reads with no
// record at all. A compare that fed the count would drop the seat from the run
// with the record.
func TestAgentBeadRetireDeadHookedCountIgnoresTheRecord(t *testing.T) {
	t.Parallel()
	town, db, batch, _ := seatShadowTown(t, "ember", "gt-work")
	db.Seed(beads.Issue{ID: "gt-work", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/ember"})
	seat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ember"}
	writeIntentRecord(t, seat, town, intent.Record{WorkBead: "gt-elsewhere"})

	withRecord, _, err := rosterOverShadowBatch(&fakeLister{}, town, batch)
	if err != nil {
		t.Fatalf("roster with the record: %v", err)
	}
	if err := os.Remove(seat.Path(town)); err != nil {
		t.Fatalf("remove the record: %v", err)
	}
	// A fresh batch, so neither run reads the other's seat cache; its lines go
	// to a buffer too, so the run is no louder than the one above.
	bare := batchOverStore(town, db)
	bare.shadow = &bytes.Buffer{}
	withoutRecord, _, err := rosterOverShadowBatch(&fakeLister{}, town, bare)
	if err != nil {
		t.Fatalf("roster without the record: %v", err)
	}

	if !reflect.DeepEqual(withRecord, withoutRecord) {
		t.Fatalf("the intent record changed the picture:\n with: %+v\n without: %+v", withRecord, withoutRecord)
	}
	counted := false
	for _, s := range withRecord {
		counted = counted || (s.rig == "gastown" && s.polecat == "ember" && s.deadHooked)
	}
	if !counted {
		t.Fatalf("the dead-hooked seat did not count with its record present: %+v", withRecord)
	}
}

// TestAgentBeadRetireDeadHookedStaysQuietWhereTheSourcesAgree: a seat whose two
// sources name the same bead, and a seat with neither, log nothing — the
// window's lines are the disagreements alone.
func TestAgentBeadRetireDeadHookedStaysQuietWhereTheSourcesAgree(t *testing.T) {
	t.Parallel()
	town, _, batch, lines := seatShadowTown(t, "ember", "gt-same")
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ember"}, town,
		intent.Record{WorkBead: "gt-same"})
	writeTownPolecatDir(t, town, "gastown", "flint")

	if _, _, err := rosterOverShadowBatch(&fakeLister{}, town, batch); err != nil {
		t.Fatalf("roster: %v", err)
	}
	if got := shadowLines(lines); len(got) != 0 {
		t.Fatalf("shadow lines = %q, want none", got)
	}
}

// TestAgentBeadRetireDeadHookedLogsAnUnreadableRecord: a record that exists and
// cannot be parsed is named as unreadable, not passed over as agreeing and not
// reported as a disagreement with a guessed value. The seat still counts on its
// bead, as it did before.
func TestAgentBeadRetireDeadHookedLogsAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	town, db, batch, lines := seatShadowTown(t, "ember", "gt-work")
	db.Seed(beads.Issue{ID: "gt-work", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/ember"})
	seat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ember"}
	if err := os.MkdirAll(filepath.Dir(seat.Path(town)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(seat.Path(town), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write the corrupt record: %v", err)
	}

	sessions, _, err := rosterOverShadowBatch(&fakeLister{}, town, batch)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	got := shadowLines(lines)
	if len(got) != 1 || !strings.HasPrefix(got[0], "agentbead_shadow: gastown/ember record unreadable, hook not compared: ") {
		t.Fatalf("shadow lines = %q, want one unreadable-record line for gastown/ember", got)
	}
	counted := false
	for _, s := range sessions {
		counted = counted || (s.rig == "gastown" && s.polecat == "ember" && s.deadHooked)
	}
	if !counted {
		t.Fatalf("an unreadable record changed the count: %+v", sessions)
	}
}

// TestAgentBeadRetireDeadHookedDoesNotReadWithoutATown: a batch built with no
// town has no record to read, and reading one anyway would resolve a path
// against the process's working directory. The seat's hook question is still
// answered, with nothing logged.
func TestAgentBeadRetireDeadHookedDoesNotReadWithoutATown(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	// The id the seat read builds with no town, seeded so the bead's hook is a
	// value the compare would report against the empty record.
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig("", "gastown"), "gastown", "ember")
	db.Seed(beads.Issue{ID: id, Title: id, Type: "agent", Labels: []string{"gt:agent"},
		Description: beads.FormatAgentDescription(id, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: "idle", HookBead: "gt-bead-hook",
		})})
	lines := &bytes.Buffer{}
	batch := batchOverStore("", db)
	batch.shadow = lines

	held, err := batch.hookHeld("gastown", "ember")
	if err != nil {
		t.Fatalf("hookHeld: %v", err)
	}
	if !held {
		t.Fatal("hookHeld dropped a seat whose hook names an unreadable bead, so the test would not see a compare")
	}
	if got := shadowLines(lines); len(got) != 0 {
		t.Fatalf("shadow lines = %q, want none without a town to read", got)
	}
}

// rosterOverShadowBatch is the seat picture the roster reads, over the batch
// whose shadow lines a test captures.
func rosterOverShadowBatch(lister sessionLister, town string, batch *poolSeatBatch) ([]poolSession, int, error) {
	return poolSeatSessionsProbedWith(lister, town, batch.disposition, batch.hookHeld, nil, seatPool("deepseek-flash", 3))
}
