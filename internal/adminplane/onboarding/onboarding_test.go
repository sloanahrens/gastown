package onboarding

import (
	"errors"
	"strings"
	"testing"
)

// errStoreFailed is what the failing key store returns, standing in for any
// store that did not write the key.
var errStoreFailed = errors.New("key store: write failed")

// doneThrough returns the state after completing steps in order, each with a key
// store that succeeds.
func doneThrough(t *testing.T, steps ...Step) State {
	t.Helper()
	s := State{}
	for _, step := range steps {
		next, err := Advance(s, step, func() error { return nil })
		if err != nil {
			t.Fatalf("Advance(%q): %v", step, err)
		}
		s = next
	}
	return s
}

// sameState reports whether a and b hold the same done and skipped steps. It
// asks each step through HasDone and HasSkipped, so a nil set and an empty one
// count as the same state.
func sameState(a, b State) bool {
	for _, step := range order {
		if a.HasDone(step) != b.HasDone(step) || a.HasSkipped(step) != b.HasSkipped(step) {
			return false
		}
	}
	return true
}

// doneCount returns how many of the steps a state marks done.
func doneCount(s State) int {
	n := 0
	for _, step := range order {
		if s.HasDone(step) {
			n++
		}
	}
	return n
}

// TestStepsCompleteInOrder walks the whole sequence and checks each step took
// the state one further.
func TestStepsCompleteInOrder(t *testing.T) {
	t.Parallel()
	s := State{}
	for i, step := range order {
		next, err := Advance(s, step, func() error { return nil })
		if err != nil {
			t.Fatalf("Advance(%q): %v", step, err)
		}
		if !next.HasDone(step) {
			t.Fatalf("state after Advance(%q) does not mark it done: %+v", step, next)
		}
		if got := doneCount(next); got != i+1 {
			t.Fatalf("state after Advance(%q) holds %d done steps, want %d", step, got, i+1)
		}
		s = next
	}
}

// TestStepsOnlyInOrder is the acceptance test for the order: a step asked for
// before its predecessor is done returns an error naming the step and what it
// needs, and leaves the state unchanged.
func TestStepsOnlyInOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		state State
		to    Step
		needs Step
	}{
		{"organization before sign-in", State{}, OrgWorkspace, SignIn},
		{"model key before the organization", doneThrough(t, SignIn), ModelKey, OrgWorkspace},
		{"repository before the model key", doneThrough(t, SignIn, OrgWorkspace), Repository, ModelKey},
		{"first project before the repository", doneThrough(t, SignIn, OrgWorkspace, ModelKey), FirstProject, Repository},
		{"gcp before the first project", doneThrough(t, SignIn, OrgWorkspace, ModelKey, Repository), GCP, FirstProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			after, err := Advance(tc.state, tc.to, func() error { return nil })
			var orderErr *OrderError
			if !errors.As(err, &orderErr) {
				t.Fatalf("Advance(%q) error = %v, want *OrderError", tc.to, err)
			}
			if orderErr.Step != tc.to || orderErr.Needs != tc.needs {
				t.Errorf("error = %+v, want step %q needing %q", orderErr, tc.to, tc.needs)
			}
			if got := err.Error(); !strings.Contains(got, string(tc.to)) || !strings.Contains(got, string(tc.needs)) {
				t.Errorf("refusal %q does not name step %q and the step it needs %q", got, tc.to, tc.needs)
			}
			if !sameState(after, tc.state) {
				t.Errorf("state = %+v after the refusal, want %+v unchanged", after, tc.state)
			}
		})
	}
}

// TestOnlyGCPMaySkipped is the acceptance test for skipping: every step but GCP
// refuses a skip, and a skipped GCP reads as skipped.
func TestOnlyGCPMaySkipped(t *testing.T) {
	t.Parallel()
	for _, step := range order {
		if step == GCP {
			continue
		}
		after, err := Skip(State{}, step)
		var notSkippable *NotSkippableError
		if !errors.As(err, &notSkippable) {
			t.Fatalf("Skip(%q) error = %v, want *NotSkippableError", step, err)
		}
		if notSkippable.Step != step {
			t.Errorf("error = %+v, want it to name step %q", notSkippable, step)
		}
		if after.HasSkipped(step) {
			t.Errorf("Skip(%q) marked it skipped despite the refusal", step)
		}
	}

	after, err := Skip(doneThrough(t, SignIn, OrgWorkspace, ModelKey, Repository, FirstProject), GCP)
	if err != nil {
		t.Fatalf("Skip(%q): %v", GCP, err)
	}
	if !after.HasSkipped(GCP) {
		t.Errorf("state after Skip(%q) does not mark it skipped: %+v", GCP, after)
	}
}

// TestSkippedStepIsNotStarted checks that a skipped step cannot then be taken.
func TestSkippedStepIsNotStarted(t *testing.T) {
	t.Parallel()
	skipped, err := Skip(doneThrough(t, SignIn, OrgWorkspace, ModelKey, Repository, FirstProject), GCP)
	if err != nil {
		t.Fatalf("Skip(%q): %v", GCP, err)
	}

	after, err := Advance(skipped, GCP, func() error { return nil })
	var settled *SettledError
	if !errors.As(err, &settled) {
		t.Fatalf("Advance(%q) error = %v, want *SettledError", GCP, err)
	}
	if !settled.Skipped || settled.Step != GCP {
		t.Errorf("error = %+v, want a skip refusal naming %q", settled, GCP)
	}
	if !sameState(after, skipped) {
		t.Errorf("state = %+v after the refusal, want %+v unchanged", after, skipped)
	}
}

// TestDoneStepIsNotRedone checks that no step may be completed twice.
func TestDoneStepIsNotRedone(t *testing.T) {
	t.Parallel()
	for i, step := range order {
		before := doneThrough(t, order[:i+1]...)
		after, err := Advance(before, step, func() error { return nil })
		var settled *SettledError
		if !errors.As(err, &settled) {
			t.Fatalf("Advance(%q) again error = %v, want *SettledError", step, err)
		}
		if settled.Skipped || settled.Step != step {
			t.Errorf("error = %+v, want a done refusal naming %q", settled, step)
		}
		if !sameState(after, before) {
			t.Errorf("state = %+v after the refusal, want %+v unchanged", after, before)
		}
	}
}

// TestUnknownStepRefused checks that a step this package does not define is
// refused by both verbs rather than treated as one no rule mentions.
func TestUnknownStepRefused(t *testing.T) {
	t.Parallel()
	const unknown Step = "teleport"
	if _, err := Advance(State{}, unknown, func() error { return nil }); !errors.Is(err, ErrUnknownStep) {
		t.Errorf("Advance(%q) error = %v, want ErrUnknownStep", unknown, err)
	}
	if _, err := Skip(State{}, unknown); !errors.Is(err, ErrUnknownStep) {
		t.Errorf("Skip(%q) error = %v, want ErrUnknownStep", unknown, err)
	}
}

// TestFailedKeyStoreLeavesModelKeyUndone is the acceptance test for the key
// store: a store that fails leaves the model-key step undone and the state
// unchanged.
func TestFailedKeyStoreLeavesModelKeyUndone(t *testing.T) {
	t.Parallel()
	before := doneThrough(t, SignIn, OrgWorkspace)
	calls := 0
	after, err := Advance(before, ModelKey, func() error {
		calls++
		return errStoreFailed
	})
	if calls != 1 {
		t.Fatalf("the key store was called %d times, want once", calls)
	}
	var storeErr *StoreKeyError
	if !errors.As(err, &storeErr) {
		t.Fatalf("Advance(%q) error = %v, want *StoreKeyError", ModelKey, err)
	}
	if !errors.Is(err, errStoreFailed) {
		t.Errorf("error = %v, want it to wrap the store's error", err)
	}
	if strings.Contains(err.Error(), errStoreFailed.Error()) {
		t.Errorf("refusal %q repeats the store's error text, which may hold the key", err)
	}
	if after.HasDone(ModelKey) {
		t.Errorf("state marks the model-key step done after the store failed: %+v", after)
	}
	if !sameState(after, before) {
		t.Errorf("state = %+v after the refusal, want %+v unchanged", after, before)
	}
}

// TestModelKeyNeedsStore checks that the model-key step does not complete with
// no store to write the key to.
func TestModelKeyNeedsStore(t *testing.T) {
	t.Parallel()
	before := doneThrough(t, SignIn, OrgWorkspace)
	after, err := Advance(before, ModelKey, nil)
	if !errors.Is(err, ErrNoKeyStore) {
		t.Fatalf("Advance(%q) with no store error = %v, want ErrNoKeyStore", ModelKey, err)
	}
	if after.HasDone(ModelKey) {
		t.Errorf("state marks the model-key step done with no store: %+v", after)
	}
}

// TestSkipDoesNotNeedPredecessor checks that skipping GCP is allowed before the
// steps ahead of it are done, since skipping does not start the step.
func TestSkipDoesNotNeedPredecessor(t *testing.T) {
	t.Parallel()
	after, err := Skip(State{}, GCP)
	if err != nil {
		t.Fatalf("Skip(%q): %v", GCP, err)
	}
	if !after.HasSkipped(GCP) {
		t.Errorf("state after Skip(%q) does not mark it skipped: %+v", GCP, after)
	}
}

// TestAdvanceReturnsNewState checks that advancing one state leaves the state
// the caller passed unchanged.
func TestAdvanceReturnsNewState(t *testing.T) {
	t.Parallel()
	before := State{}
	after, err := Advance(before, SignIn, func() error { return nil })
	if err != nil {
		t.Fatalf("Advance(%q): %v", SignIn, err)
	}
	if before.HasDone(SignIn) {
		t.Errorf("the state passed to Advance changed: %+v", before)
	}
	if !after.HasDone(SignIn) {
		t.Errorf("the state Advance returned does not mark %q done: %+v", SignIn, after)
	}
}

// TestSameStateComparesSteps checks that sameState calls two states the same
// when they mark the same steps, whether or not their sets are allocated, and
// different when they differ in a done or a skipped step.
func TestSameStateComparesSteps(t *testing.T) {
	t.Parallel()
	signedIn := doneThrough(t, SignIn)
	cases := []struct {
		name string
		a, b State
		want bool
	}{
		{"zero value and empty sets", State{}, State{done: set{}, skipped: set{}}, true},
		{"same done step", signedIn, doneThrough(t, SignIn), true},
		{"extra done step", signedIn, doneThrough(t, SignIn, OrgWorkspace), false},
		{"skipped differs from done", signedIn, State{skipped: set{OrgWorkspace: true}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sameState(tc.a, tc.b); got != tc.want {
				t.Errorf("sameState(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
