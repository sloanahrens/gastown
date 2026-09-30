package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// dogFormulaHarness is a harness whose target resolution is the real one, so
// a dog target dispatches through the fake dog pool.
func dogFormulaHarness(t *testing.T) *slingHarness {
	t.Helper()
	h := newSlingHarness(t)
	h.run.resolveTarget = h.run.resolveSlingTarget
	return h
}

// indexOf is the position of call in the log, or -1.
func (h *slingHarness) indexOf(call string) int {
	for i, c := range h.log() {
		if c == call {
			return i
		}
	}
	return -1
}

// TestRunSlingFormulaCleansDelayedDogFailure: a delayed dog's formula sling
// that fails after creating its wisp burns that wisp and clears the dog's
// assignment, and does both while it still holds the assignee lock.
func TestRunSlingFormulaCleansDelayedDogFailure(t *testing.T) {
	t.Parallel()
	h := dogFormulaHarness(t)
	h.run.startDelayedDog = func(*DogDispatchInfo) (string, error) { return "", errors.New("tmux down") }

	err := h.run.runFormula(context.Background(), []string{"mol-dog-reaper", "deacon/dogs/alpha"})
	wantSlingErr(t, err, "starting delayed dog session")
	burn, clear, unlock := h.indexOf("cleanup dog wisp gt-wisp-new"), h.indexOf("clear dog work alpha"), h.indexOf("unlock assignee deacon/dogs/alpha")
	if burn < 0 || clear < 0 || unlock < 0 || burn > unlock || clear > unlock {
		t.Fatalf("dog cleanup must burn the wisp and clear the work before the unlock\nlog:\n  %s", strings.Join(h.log(), "\n  "))
	}
}

// TestCleanupDelayedDogFormulaFailureClearsWorkAfterWispCleanupError: a
// failed wisp burn does not stop the dog's assignment being cleared, and the
// error carries both the sling's failure and the burn's.
func TestCleanupDelayedDogFormulaFailureClearsWorkAfterWispCleanupError(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.cleanupFailedDogWisp = func(string, string) error { return errors.New("close failed") }
	dog := &DogDispatchInfo{DogName: "alpha", workDesc: "mol-dog-reaper", ownsWork: true}

	err := h.run.cleanupDelayedDogFormulaFailure(errors.New("start failed"), dog, "gt-wisp", slingTestTown)
	if err == nil || !strings.Contains(err.Error(), "start failed") || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("cleanup error = %v, want joined start and close errors", err)
	}
	h.wantCalls("clear dog work", "clear dog work alpha")
}

// TestRunSlingFormulaSerializesWholeDogPool: a formula slung to the dog pool
// takes one pool-wide lock before choosing a dog, not a per-formula lock.
func TestRunSlingFormulaSerializesWholeDogPool(t *testing.T) {
	t.Parallel()
	h := dogFormulaHarness(t)

	if err := h.run.runFormula(context.Background(), []string{"mol-dog-reaper", "deacon/dogs"}); err != nil {
		t.Fatalf("runFormula: %v", err)
	}
	h.wantCalls("lock assignee", "lock assignee deacon/dogs", "lock assignee deacon/dogs/alpha")
	if h.indexOf("lock assignee deacon/dogs") > h.indexOf("dispatch dog alpha") {
		t.Fatalf("the pool lock must be held before a dog is chosen\nlog:\n  %s", strings.Join(h.log(), "\n  "))
	}
}

// reusedDog is a dog already working on formula, dispatched without owning
// the work, and the wisp it has hooked.
func reusedDog(formula string) (*DogDispatchInfo, *beads.Issue) {
	started := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	dog := &DogDispatchInfo{DogName: "alpha", AgentID: "deacon/dogs/alpha", sessionDelayed: true,
		workDesc: formula, workStartedAt: started}
	wisp := &beads.Issue{ID: "gt-wisp-existing",
		Description: "attached_formula: " + formula + "\nattached_at: " + started.Add(time.Minute).Format(time.RFC3339Nano)}
	return dog, wisp
}

// TestRunSlingFormulaExistingHookedDogStartsDelayedSession: a dog reused for
// the formula it already has hooked is a no-op for the wisp, but its delayed
// session is still started and nudged before the sling returns.
func TestRunSlingFormulaExistingHookedDogStartsDelayedSession(t *testing.T) {
	t.Parallel()
	h := dogFormulaHarness(t)
	dog, wisp := reusedDog("mol-dog-reaper")
	h.run.dispatchDog = func(string, DogDispatchOptions) (*DogDispatchInfo, error) { return dog, nil }
	h.hookedFormulas["deacon/dogs/alpha"] = wisp

	if err := h.run.runFormula(context.Background(), []string{"mol-dog-reaper", "deacon/dogs/alpha"}); err != nil {
		t.Fatalf("runFormula: %v", err)
	}
	start, nudge := h.indexOf("start dog alpha"), -1
	for i, c := range h.log() {
		if strings.HasPrefix(c, "nudge session hq-dog-alpha:") {
			nudge = i
		}
	}
	if start < 0 || nudge < start {
		t.Fatalf("the reused dog must be started, then nudged\nlog:\n  %s", strings.Join(h.log(), "\n  "))
	}
	h.wantNo("create wisp")
	h.wantNo("clear dog work")
}

// TestRunSlingFormulaNonOwnedDogReuseCannotCreateFreshWisp: a dog dispatched
// to reuse work it does not own, whose wisp is gone by the time it is
// checked, aborts before creating a fresh wisp.
func TestRunSlingFormulaNonOwnedDogReuseCannotCreateFreshWisp(t *testing.T) {
	t.Parallel()
	h := dogFormulaHarness(t)
	dog, _ := reusedDog("mol-dog-reaper")
	h.run.dispatchDog = func(string, DogDispatchOptions) (*DogDispatchInfo, error) { return dog, nil }

	err := h.run.runFormula(context.Background(), []string{"mol-dog-reaper", "deacon/dogs/alpha"})
	wantSlingErr(t, err, "dog formula reuse became stale")
	h.wantNo("cook")
	h.wantNo("create wisp")
}

// TestRunSlingFormulaDogNudgeBeforeEmptyPaneReturn: a dog's session is
// nudged by session name even when starting it reported no pane, rather than
// taking the generic "no pane to nudge" return.
func TestRunSlingFormulaDogNudgeBeforeEmptyPaneReturn(t *testing.T) {
	t.Parallel()
	h := dogFormulaHarness(t)
	h.run.startDelayedDog = func(dog *DogDispatchInfo) (string, error) {
		h.record("start dog %s", dog.DogName)
		return "", nil
	}

	if err := h.run.runFormula(context.Background(), []string{"mol-dog-reaper", "deacon/dogs/alpha"}); err != nil {
		t.Fatalf("runFormula: %v", err)
	}
	if len(h.matching("nudge session hq-dog-alpha:")) != 1 {
		t.Fatalf("the dog was not nudged by session\nlog:\n  %s", strings.Join(h.log(), "\n  "))
	}
	if strings.Contains(h.out.String(), "No pane to nudge") {
		t.Fatal("the dog took the empty-pane return instead of its session nudge")
	}
}
