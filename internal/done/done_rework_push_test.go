package done

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// The submit path's rework push, end to end through submitForLanding: the
// source bead's rework label and rejection note reach pushBranchToOrigin, so a
// content-changing rework over the rejected head is pushed under a lease
// pinned to that head. The unit tests above call recoverDivergedPush with a
// reworkPush built by hand; these pin the wiring that builds it.

const (
	reworkWiringRejected = "aaaa1111bbbb2222cccc3333dddd4444eeee5555"
	reworkWiringLocal    = "feature1"
	reworkWiringSeat     = "refuge"
	reworkWiringEmail    = "seat@test.com"
)

// reworkWiringHarness is a submit whose source bead is a rework of
// reworkWiringRejected, with origin holding origin's submission at that head.
// origin's commit is written by author at email. The local rework changes the
// content, so its patch-id is not origin's.
func reworkWiringHarness(t *testing.T, author, email string) *submitHarness {
	t.Helper()
	h := newSubmitHarness(t)
	h.r.polecatName = reworkWiringSeat
	h.repo.authorEmail = reworkWiringEmail
	h.repo.origin[doneTestBranch] = reworkWiringRejected
	h.repo.patchIDs[reworkWiringRejected] = []string{"p-origin"}
	h.repo.patchIDs[reworkWiringLocal] = []string{"p-local"}
	rejected := []git.CommitSummary{{SHA: reworkWiringRejected, Author: author, Email: email, Subject: "feat: the rejected submission"}}
	h.repo.summaries = map[string][]git.CommitSummary{reworkWiringRejected: rejected}
	// The rejected head is a linear one-commit range here, so both walks agree.
	// They are recorded apart: the case that tells them apart is
	// TestSubmitReworkForeignSecondParentRefusesFromBead.
	h.repo.fullSummaries = map[string][]git.CommitSummary{reworkWiringRejected: rejected}
	h.bd.Seed(beads.Issue{
		ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusHooked),
		Labels: []string{land.LabelRework},
		Notes:  reworkStandDownNote("", doneTestBranch, reworkWiringRejected),
	})
	return h
}

// The rejected submission's commit was authored under the seat's own
// gastown.local address: the rework lease accepts it, as it accepts the
// worktree's email.
func TestSubmitReworkUnderSeatLocalAddressPushesFromBead(t *testing.T) {
	t.Parallel()
	h := reworkWiringHarness(t, reworkWiringSeat, reworkWiringSeat+"@gastown.local")
	if err := h.submit(); err != nil {
		t.Fatalf("submit = %v, want the rework to land over the gastown.local commit", err)
	}
	if got := h.repo.origin[doneTestBranch]; got != reworkWiringLocal {
		t.Errorf("origin %s = %s, want the rework %s", doneTestBranch, got, reworkWiringLocal)
	}
}

// The rejected submission's commit was authored under the host's global email,
// the legacy address. The lease accepts it only because the globalEmail seam
// reports it.
func TestSubmitReworkUnderLegacyGlobalEmailPushesFromBead(t *testing.T) {
	t.Parallel()
	const legacy = "host@test.com"
	h := reworkWiringHarness(t, reworkWiringSeat, legacy)
	h.r.deps.globalEmail = func() string { return legacy }
	if err := h.submit(); err != nil {
		t.Fatalf("submit = %v, want the rework to land over the legacy-email commit", err)
	}
	if got := h.repo.origin[doneTestBranch]; got != reworkWiringLocal {
		t.Errorf("origin %s = %s, want the rework %s", doneTestBranch, got, reworkWiringLocal)
	}
}

// With no global email to read (the seam reports unset), the same legacy-email
// commit is another address and the rework is refused.
func TestSubmitReworkUnderLegacyEmailWithUnsetGlobalRefusesFromBead(t *testing.T) {
	t.Parallel()
	h := reworkWiringHarness(t, reworkWiringSeat, "host@test.com")
	h.r.deps.globalEmail = func() string { return "" }
	if err := h.submit(); err == nil {
		t.Fatal("submit succeeded over a commit under an address the seat does not own")
	}
	if got := h.repo.origin[doneTestBranch]; got != reworkWiringRejected {
		t.Errorf("origin %s = %s, want it untouched at %s", doneTestBranch, got, reworkWiringRejected)
	}
}

// A push that is not a rework never consults the host's global email: only a
// rework lease can match it, so the read is skipped for every other push.
func TestSubmitNonReworkPushDoesNotReadGlobalEmail(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	reads := 0
	h.r.deps.globalEmail = func() string {
		reads++
		return "host@test.com"
	}
	if err := h.submit(); err != nil {
		t.Fatalf("submit = %v, want the plain push to land", err)
	}
	if reads != 0 {
		t.Errorf("globalEmail read %d time(s) on a non-rework push, want 0", reads)
	}
}

func TestSubmitReworkOfRejectedHeadPushesUnderLeaseFromBead(t *testing.T) {
	t.Parallel()
	h := reworkWiringHarness(t, reworkWiringSeat, reworkWiringEmail)
	if err := h.submit(); err != nil {
		t.Fatalf("submit = %v, want the rework to land on origin", err)
	}
	want := doneTestBranch + " " + reworkWiringLocal + " lease=" + reworkWiringRejected
	if len(h.repo.pushes) == 0 || h.repo.pushes[len(h.repo.pushes)-1] != want {
		t.Fatalf("pushes = %q, want the last one to be %q", h.repo.pushes, want)
	}
	if got := h.repo.origin[doneTestBranch]; got != reworkWiringLocal {
		t.Errorf("origin %s = %s, want the rework %s", doneTestBranch, got, reworkWiringLocal)
	}
}

func TestSubmitReworkForeignAuthorOnOriginRefusesFromBead(t *testing.T) {
	t.Parallel()
	h := reworkWiringHarness(t, "Sloan Ahrens", reworkWiringEmail)
	if err := h.submit(); err == nil {
		t.Fatal("submit succeeded over another author's commit on origin")
	}
	for _, p := range h.repo.pushes {
		if strings.Contains(p, "lease="+reworkWiringRejected) {
			t.Errorf("lease push ran over the foreign author's commit: %q", p)
		}
	}
	if got := h.repo.origin[doneTestBranch]; got != reworkWiringRejected {
		t.Errorf("origin %s = %s, want it untouched at %s", doneTestBranch, got, reworkWiringRejected)
	}
}

// TestSubmitReworkForeignSecondParentRefusesFromBead: the rejected head is a
// seat-authored merge. Its first-parent chain is all the seat's, but its second
// parent carries another author's commit, so the full-range walk is what
// refuses the overwrite. The fake records the two walks separately, so a path
// that read the first-parent listing would land this push and fail the test.
func TestSubmitReworkForeignSecondParentRefusesFromBead(t *testing.T) {
	t.Parallel()
	h := reworkWiringHarness(t, reworkWiringSeat, reworkWiringEmail)
	h.repo.fullSummaries[reworkWiringRejected] = []git.CommitSummary{
		{SHA: reworkWiringRejected, Author: reworkWiringSeat, Email: reworkWiringEmail, Subject: "Merge branch 'side'"},
		{SHA: "s1", Author: "Sloan Ahrens", Email: reworkWiringEmail, Subject: "fix: the foreign commit"},
		{SHA: "p1", Author: reworkWiringSeat, Email: reworkWiringEmail, Subject: "feat: the first parent"},
	}
	if err := h.submit(); err == nil {
		t.Fatal("submit succeeded over a foreign commit reachable only through the merge's second parent")
	}
	for _, p := range h.repo.pushes {
		if strings.Contains(p, "lease="+reworkWiringRejected) {
			t.Errorf("lease push ran over the foreign commit: %q", p)
		}
	}
	if got := h.repo.origin[doneTestBranch]; got != reworkWiringRejected {
		t.Errorf("origin %s = %s, want it untouched at %s", doneTestBranch, got, reworkWiringRejected)
	}
}
