package done

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/land/landtest"
)

// TestSubmitIgnoresAnUnansweredLocalRoundMajor pins gt-6qb84r in the unit
// tier: the operator retired the polecat's local review round on 2026-11-14,
// so gt done no longer reads LOCAL ROUND blocks. A work bead whose notes carry
// a round with an unanswered major — the state that refused a submit while the
// gate existed (gt-mxz8kx) — submits like any other bead: the branch is pushed
// and the work bead is marked ready to land naming that head.
//
// The end-to-end run of the same behaviour is
// TestIntegrationRunDoneIgnoresAnUnansweredLocalRoundMajor in internal/cmd: it
// drives runDone against real git and a bd stub, so it stays in the
// integration tier, which the gate does not run: the realgit ratchet does not
// admit a new unit-tier test file that execs git (internal/testpolicy/realgit.txt
// only shrinks). This copy drives the submit path the retired gate preceded, in
// process, so make gate sees the behaviour too.
func TestSubmitIgnoresAnUnansweredLocalRoundMajor(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)

	h.bd.Seed(beads.Issue{
		ID:     "bd-source",
		Title:  "the work",
		Type:   "task",
		Status: string(beads.StatusHooked),
		Notes:  landtest.LocalRoundWithMajor,
	})

	if err := h.submit(); err != nil {
		t.Fatalf("submit with an unanswered LOCAL ROUND major in the work bead's notes: %v", err)
	}

	// The submission went through: origin holds the branch at HEAD, and the
	// work bead carries the READY TO LAND note naming that head.
	head := h.repo.head
	if got := h.repo.origin[doneTestBranch]; got != head {
		t.Errorf("origin/%s = %q, want the pushed head %s", doneTestBranch, got, head)
	}
	issue := h.source(t)
	w, ok := land.ParseReadyNote(issue.Notes)
	if !ok || w.Head != head || w.Branch != doneTestBranch {
		t.Errorf("READY TO LAND note = %+v (parsed %v) from %q", w, ok, issue.Notes)
	}

	// The round was in front of the submit and was ignored rather than never
	// seen: the notes still carry it beside the READY TO LAND note.
	if !strings.Contains(issue.Notes, "LOCAL ROUND (1)") {
		t.Errorf("the work bead's notes lost the LOCAL ROUND block, so the fixture proves nothing:\n%s", issue.Notes)
	}
}
