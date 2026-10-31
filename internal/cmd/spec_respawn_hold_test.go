package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// A bead a kill at the bound left labeled is retried only while its respawn
// budget lasts. The spawn refuses a bead at the limit after the sling has paid
// for its duplicate check and seat decision, so a retry of one is a sling that
// cannot succeed (gt-s6a0i).

func respawnRetryBoard() func(string) ([]*beads.Issue, error) {
	return func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{
			{ID: "gt-retry", Type: "task", Status: "open", Priority: 2, Labels: []string{specdispatch.DispatchFailedLabel}},
		}, nil
	}
}

func notDocked(string, string) (bool, string) { return false, "" }

// The bead is at its limit: the retry set names it, but the read leaves the
// label on, plans no sling and reports the bead as held by the respawn limit.
func TestTimeoutRetrySkipsABeadAtItsRespawnLimit(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	var cleared []string
	retry := &specTimeoutRetry{
		due: func(string) (map[string]bool, error) { return map[string]bool{"gt-retry": true}, nil },
		clear: func(_, beadID string) error {
			cleared = append(cleared, beadID)
			return nil
		},
		respawnLimited: func(string) bool { return true },
	}

	got := specCandidatesRetrying(townRoot, 2, respawnRetryBoard(), notDocked, retry)

	if len(cleared) != 0 {
		t.Errorf("cleared %v, want the label left on a bead at its respawn limit", cleared)
	}
	if len(got.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none", got.Candidates)
	}
	if got.LabeledFailed != 1 {
		t.Errorf("LabeledFailed = %d, want the held bead counted", got.LabeledFailed)
	}
	if len(got.RespawnHeld) != 1 || got.RespawnHeld[0] != "gt-retry" {
		t.Errorf("RespawnHeld = %v, want gt-retry", got.RespawnHeld)
	}
	if len(got.Retried) != 0 {
		t.Errorf("Retried = %v, want none", got.Retried)
	}
}

// Below the limit nothing changes: the label is cleared and the bead is slung.
func TestTimeoutRetryStillRetriesBelowTheLimit(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	var cleared []string
	retry := &specTimeoutRetry{
		due: func(string) (map[string]bool, error) { return map[string]bool{"gt-retry": true}, nil },
		clear: func(_, beadID string) error {
			cleared = append(cleared, beadID)
			return nil
		},
		respawnLimited: func(string) bool { return false },
	}

	got := specCandidatesRetrying(townRoot, 2, respawnRetryBoard(), notDocked, retry)

	if len(got.Candidates) != 1 || got.Candidates[0].Spec.ID != "gt-retry" {
		t.Fatalf("candidates = %+v, want the retried bead", got.Candidates)
	}
	if len(cleared) != 1 || len(got.RespawnHeld) != 0 {
		t.Errorf("cleared = %v, RespawnHeld = %v, want one clear and nothing held", cleared, got.RespawnHeld)
	}
}

// Three refusals produce no fourth sling. The respawn state is the real one:
// the bead spends its budget through RecordBeadRespawn, and four ticks over the
// same board plan a sling for it only while the budget lasts, never after the
// third respawn. Reading the limit as always false lets the fourth tick plan it
// again and fails this test.
func TestThreeRefusalsProduceNoFourthSling(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	cleared := 0
	retry := &specTimeoutRetry{
		due:            func(string) (map[string]bool, error) { return map[string]bool{"gt-retry": true}, nil },
		clear:          func(string, string) error { cleared++; return nil },
		respawnLimited: specRespawnLimitedIn(townRoot),
	}

	var planned []int
	for tick := 1; tick <= 4; tick++ {
		got := specCandidatesRetrying(townRoot, 2, respawnRetryBoard(), notDocked, retry)
		planned = append(planned, len(got.Candidates))
		if len(got.Candidates) == 1 {
			// The sling the tick planned spawns a polecat, which spends one
			// respawn; the first two spawns leave budget, the third spends it.
			if _, err := polecat.RecordBeadRespawn(townRoot, "gt-retry"); err != nil {
				t.Fatalf("RecordBeadRespawn: %v", err)
			}
		}
	}

	want := []int{1, 1, 1, 0}
	for i := range want {
		if planned[i] != want[i] {
			t.Fatalf("slings planned per tick = %v, want %v: no fourth sling once the budget is spent", planned, want)
		}
	}
	if cleared != 3 {
		t.Errorf("label cleared %d times, want 3", cleared)
	}
}

// The tick reports the held beads in one line that names each once.
func TestTickLineNamesBeadsHeldByTheRespawnLimit(t *testing.T) {
	t.Parallel()
	if got := specRespawnHeldNotice(nil); got != "" {
		t.Errorf("notice for no held bead = %q, want none", got)
	}
	got := specRespawnHeldNotice([]string{"gt-a", "gt-b"})
	for _, want := range []string{"2 bead(s) held by the respawn limit", "gt-a gt-b", "gt sling respawn-reset"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q lacks %q", got, want)
		}
	}
	if strings.Count(got, "gt-a") != 1 {
		t.Errorf("notice %q names gt-a more than once", got)
	}
}
