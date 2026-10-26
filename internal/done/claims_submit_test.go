package done

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The fake repo reads the branch's commit subjects and numstat for the flag.
func (f *fakeDoneRepo) CommitSubjects(base, head string) ([]string, error) {
	return []string{"fix the landing tick"}, nil
}

func (f *fakeDoneRepo) DiffNumstat(base, head string) (string, error) {
	return "3\t1\tinternal/done/done.go\n", nil
}

// A claims battery that cannot answer does not stop gt done from submitting: the
// branch is pushed and the source bead is marked ready, and the battery was asked
// (gt-b9j3o.11).
func TestSubmitStillLandsWhenTheClaimsBatteryCannotAnswer(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.behind = 1
	battery := &fakeClaimsBattery{err: errors.New("jev unreachable")}
	h.r.deps.claims = battery

	if err := h.submit(); err != nil {
		t.Fatalf("submit with a failing claims battery: %v", err)
	}
	if !battery.asked {
		t.Fatal("the claims battery was never asked, so the flag path did not run")
	}
	if got := h.repo.origin[doneTestBranch]; got != "feature1-rebased" {
		t.Errorf("origin/%s = %q, want the pushed head", doneTestBranch, got)
	}
	issue := h.source(t)
	if issue.Status == string(beads.StatusClosed) {
		t.Error("the source bead closed; gt done must leave closing to the landing worker")
	}
}
