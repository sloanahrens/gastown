package refinery

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestLandConvoySwarmReportsBranchNotLanded: when a closed convoy's molecule
// has a swarm/<mol> integration branch, the engineer used to exec
// `gt swarm land`, a command gt no longer has, and log the failure as a
// Warning (gt-fcxe9.5, deep review G4-03). It must not exec gt at all, and
// must say plainly that the branch was not landed.
func TestLandConvoySwarmReportsBranchNotLanded(t *testing.T) {
	_, gtLog := fakeBDAndGt(t)
	workDir, g, _ := testGitRepo(t)
	if out, err := exec.Command("git", "-C", workDir, "branch", "swarm/mol-x").CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	e := newTestEngineer(t, workDir, g)

	e.landConvoySwarm(convoyInfo{ID: "hq-cv-1", Description: "Convoy tracking 1 issue\nMolecule: mol-x"})

	if calls := readLog(t, gtLog); calls != "" {
		t.Errorf("landConvoySwarm exec'd gt: %q", calls)
	}
	out := e.output.(*bytes.Buffer).String()
	for _, want := range []string{"swarm/mol-x", "hq-cv-1", "NOT landed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// A log line alone strands the branch: the operator must be told.
	got := recorderOf(t, e).Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want one", got)
	}
	esc := got[0].Escalation
	if strings.ContainsAny(esc.Description, "\r\n") || !strings.Contains(esc.Description, "swarm/mol-x") || !strings.Contains(esc.Description, "hq-cv-1") {
		t.Errorf("description = %q, want one line naming the branch and convoy", esc.Description)
	}
	if esc.Fingerprint == "" || !strings.Contains(esc.Fingerprint, "mol-x") {
		t.Errorf("fingerprint = %q, want one keyed on the molecule so repeats fold", esc.Fingerprint)
	}
}
