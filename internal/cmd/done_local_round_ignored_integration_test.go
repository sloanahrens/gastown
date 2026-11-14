//go:build integration

package cmd

import (
	"strings"
	"testing"
)

// localRoundWithMajor is the note the retired local round formatter wrote for
// round 1 of head abcdef1: one major finding that no reply answers. The
// formatter left with the round (gt-755l63), so the fixture is the literal
// bytes it wrote; beads that ran a round before 2026-11-14 still carry theirs.
const localRoundWithMajor = "LOCAL ROUND (1): head=abcdef1 at=2026-11-13T02:30:00Z\n" +
	"LOCAL ROUND FINDING id:f1 sev:major internal/x/a.go:12 — unbounded poll loop"

// TestIntegrationRunDoneIgnoresAnUnansweredLocalRoundMajor pins gt-6qb84r: the
// operator retired the polecat's local review round on 2026-11-14, so gt done
// no longer reads LOCAL ROUND blocks. A work bead whose notes carry a round
// with an unanswered major — the state that refused a submit while the gate
// existed (gt-mxz8kx) — submits like any other bead: the branch is on origin
// and the work bead is marked ready to land.
func TestIntegrationRunDoneIgnoresAnUnansweredLocalRoundMajor(t *testing.T) {
	r := runDoneSubmitRecording(t, passingDoneGate(), nil,
		func(t *testing.T, workDir string) { setupRoutedSubmitGitRepo(t, workDir, false) },
		func(t *testing.T, currentBeadsDir, ownerBeadsDir string) string {
			return installSubmitSourceBDRecorderWithNotes(t, currentBeadsDir, ownerBeadsDir, localRoundWithMajor)
		})
	if r.err != nil {
		t.Fatalf("gt done with an unanswered LOCAL ROUND major in the work bead's notes: %v", r.err)
	}

	// The submit went through: origin holds the branch at HEAD, and the bead
	// carries the one READY TO LAND update naming that head.
	head := gitOut(t, r.workDir, "rev-parse", "HEAD")
	if got := strings.Fields(gitOut(t, r.workDir, "ls-remote", "origin", "refs/heads/"+doneTestBranch)); len(got) == 0 || got[0] != head {
		t.Fatalf("origin/%s = %v, want the pushed HEAD %s", doneTestBranch, got, head)
	}
	if ready := readyToLandUpdate(t, r.bdLog); !strings.Contains(ready, "Head: "+head) {
		t.Errorf("the ready-to-land update does not name the pushed head %s:\n%s", head, ready)
	}

	// The run read the work bead, so the round was in front of it and was
	// ignored rather than never seen.
	if !strings.Contains(r.bdLog, "show bd-source") {
		t.Fatalf("gt done never read the work bead, so the notes fixture proves nothing:\n%s", r.bdLog)
	}
}
