package cmd

import (
	"io"
	"strings"
	"testing"
)

// reassignmentFixture is a town whose routes send gt-zd7c to the gastown rig,
// and an in-process bd that logs every call as "<cmd> <args...>".
func reassignmentFixture(t *testing.T) (townRoot string, bd *inprocBD) {
	t.Helper()
	townRoot = t.TempDir()
	writeGastownRoutes(t, townRoot)
	bd = &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		return bdOut("[]")
	}}
	return townRoot, bd
}

// TestRecordReassignmentCarriesOutgoingAssignee is the gt-zd7c fix: the bead
// itself must name the polecat whose assignee the sling overwrote, or an audit
// that starts from the bead has no way back to the earlier branch. Branch
// enumeration that cannot tell still writes the record.
func TestRecordReassignmentCarriesOutgoingAssignee(t *testing.T) {
	t.Parallel()
	townRoot, bd := reassignmentFixture(t)
	var branchesFor string
	branches := func(tr, beadID string) []string {
		branchesFor = beadID
		if tr != townRoot {
			t.Errorf("branches asked of town %q, want %q", tr, townRoot)
		}
		return nil
	}

	recordReassignmentVia(bd.run, branches, io.Discard, townRoot, "gt-zd7c", "gastown/polecats/jasper", "gastown/polecats/obsidian", "mayor")

	log := bd.log()
	for _, want := range []string{
		"comments add gt-zd7c",
		"REASSIGNED: gastown/polecats/jasper -> gastown/polecats/obsidian",
		"Branch: (unknown)",
		"By: mayor",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("reassignment record missing %q; got: %q", want, log)
		}
	}
	if branchesFor != "gt-zd7c" {
		t.Errorf("surviving branches listed for %q, want gt-zd7c", branchesFor)
	}
}

// TestRecordReassignmentSkipsNoOps guards the write budget: Dolt takes a
// permanent commit per comment, and neither an unassigned bead nor an
// unchanged assignee has anything to record.
func TestRecordReassignmentSkipsNoOps(t *testing.T) {
	t.Parallel()
	townRoot, bd := reassignmentFixture(t)
	branches := func(string, string) []string {
		t.Error("a no-op reassignment must not list branches")
		return nil
	}

	recordReassignmentVia(bd.run, branches, io.Discard, townRoot, "gt-zd7c", "", "gastown/polecats/obsidian", "mayor")
	recordReassignmentVia(bd.run, branches, io.Discard, townRoot, "gt-zd7c", "gastown/polecats/obsidian", "gastown/polecats/obsidian", "mayor")

	if got := bd.log(); got != "" {
		t.Errorf("expected no bd call for a no-op reassignment, got: %q", got)
	}
}
