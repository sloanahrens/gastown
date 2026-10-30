package beads

import (
	"errors"
	"testing"
)

// depListUpAnswer is what bd 1.2.2 answered in machine mode for
// `dep list --direction=up` on a bead carrying a molecule bond: the durable
// dependents and the bonded wisp, each with its relation, inside the machine
// envelope.
const depListUpAnswer = `{"schema_version":1,"contract_version":1,"data":[` +
	`{"id":"gt-b","title":"blocked","status":"open","priority":2,"issue_type":"task","dependency_type":"blocks"},` +
	`{"id":"gt-wisp-1","title":"mol-polecat-work","status":"open","priority":2,"issue_type":"molecule","ephemeral":true,"dependency_type":"blocks"}` +
	`]}`

// TestDepListUpAsksForTheUpDirection covers the bd call behind DepListUp and
// the parse of its answer: the dependents, the wisp among them, each carrying
// its relation, and the type filter reaching bd as --type.
func TestDepListUpAsksForTheUpDirection(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply { return reply{stdout: depListUpAnswer} })
	bd := newRecordedBeads(t.TempDir(), r)

	deps, err := bd.DepListUp("gt-a", "blocks")
	if err != nil {
		t.Fatalf("DepListUp: %v", err)
	}
	if len(deps) != 2 || deps[0].ID != "gt-b" || deps[1].ID != "gt-wisp-1" || deps[1].DependencyType != "blocks" || deps[1].Type != "molecule" {
		t.Errorf("DepListUp = %+v, want the durable dependent and the wisp, each blocks", deps)
	}
	if got := r.argvs()[0]; got != "dep list gt-a --direction=up --json --type=blocks" {
		t.Errorf("bd argv = %q, want the up direction and the type filter", got)
	}

	deps, err = bd.DepListUp("gt-a", "")
	if err != nil || len(deps) != 2 {
		t.Errorf("DepListUp(any relation) = %d rows, %v; want both", len(deps), err)
	}
	if got := r.argvs()[1]; got != "dep list gt-a --direction=up --json" {
		t.Errorf("bd argv without a relation = %q, want no --type", got)
	}
}

// TestDepListUpRefusesAFailedRead covers the answer bd cannot give: an exit
// failure is an error, and no output at all is no dependents.
func TestDepListUpRefusesAFailedRead(t *testing.T) {
	t.Parallel()
	failure := newRecorder(func([]string) reply { return reply{stderr: "database is locked", err: exitError{1}} })
	if _, err := newRecordedBeads(t.TempDir(), failure).DepListUp("gt-a", ""); err == nil {
		t.Error("DepListUp succeeded on a failed bd read")
	} else if errors.Is(err, ErrNotFound) {
		t.Errorf("a failed read looks like a missing issue: %v", err)
	}

	empty := newRecorder(func([]string) reply { return reply{} })
	if deps, err := newRecordedBeads(t.TempDir(), empty).DepListUp("gt-a", ""); err != nil || len(deps) != 0 {
		t.Errorf("DepListUp of empty output = %+v, %v; want no dependents", deps, err)
	}
}
