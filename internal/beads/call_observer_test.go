package beads

import (
	"context"
	"sync"
	"testing"
)

// countingRunner is an injected bd seam that records every invocation and
// answers with an empty body, so a test can compare what the seam saw with
// what the wrapper observed. The --allow-stale probe reaches it directly
// (allowStaleArgs), which is how a test tells probe invocations apart from the
// calls the wrapper makes for its caller.
type countingRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *countingRunner) run(_ context.Context, args []string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	// An empty JSON array is bd's answer for a read with no rows, and the
	// parsers the tests reach accept it. The --allow-stale probe reads it as
	// an empty version, which keeps the flag off the call.
	return []byte("[]"), nil, nil
}

func (r *countingRunner) seen() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

// probes counts the --allow-stale capability probes the seam answered: the
// probe is the fixed "bd --allow-stale version" argv, which a real call only
// carries as its first two flags.
func (r *countingRunner) probes() int {
	n := 0
	for _, args := range r.seen() {
		if len(args) == 2 && args[0] == "--allow-stale" && args[1] == "version" {
			n++
		}
	}
	return n
}

// TestSlingCallsCountEveryBDCallOnce: the observer the spec dispatcher hands a
// store with WithCallObserver runs once for each bd call the store makes, on
// both run paths — the policy path (a routed store) and the plain path
// (NewPlain) — and not for the --allow-stale capability probe, which asks what
// the bd binary accepts rather than doing work for the caller.
//
// The name is the gate's: `go test ./internal/beads -run SlingCalls` must run
// the observer tests without widening the gate's pattern to every test here.
func TestSlingCallsCountEveryBDCallOnce(t *testing.T) {
	t.Parallel()
	t.Run("policy path", func(t *testing.T) {
		t.Parallel()
		runner := &countingRunner{}
		var mu sync.Mutex
		count := 0
		observe := func() { mu.Lock(); count++; mu.Unlock() }
		b := New(t.TempDir(), WithRunner(runner.run), WithCallObserver(observe))

		for i := 0; i < 3; i++ {
			if _, err := b.Comments("gt-abc"); err != nil {
				t.Fatalf("Comments: %v", err)
			}
		}

		if count != 3 {
			t.Errorf("observer ran %d times for 3 calls, want 3", count)
		}
		// The probe runs through exec directly: it must not be counted. The
		// seam is asked once per call plus its probe.
		if want := 3 + runner.probes(); len(runner.seen()) != want {
			t.Errorf("seam saw %d invocations (%d probes), want %d", len(runner.seen()), runner.probes(), want)
		}
		if runner.probes() == 0 {
			t.Error("the seam saw no --allow-stale probe, so the test cannot tell a probe from a counted call")
		}
	})

	t.Run("plain path", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		count := 0
		observe := func() { mu.Lock(); count++; mu.Unlock() }
		b := NewPlain(t.TempDir(), nil, WithCallObserver(observe), WithRunner(func(context.Context, []string) ([]byte, []byte, error) {
			return []byte("[]"), nil, nil
		}))

		for i := 0; i < 2; i++ {
			if _, err := b.Comments("gt-abc"); err != nil {
				t.Fatalf("Comments: %v", err)
			}
		}

		if count != 2 {
			t.Errorf("observer ran %d times for 2 plain calls, want 2", count)
		}
	})

	t.Run("a call bd is asked twice for counts twice", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		count := 0
		observe := func() { mu.Lock(); count++; mu.Unlock() }
		b := NewPlain(t.TempDir(), nil, WithCallObserver(observe), WithRunner(func(_ context.Context, args []string) ([]byte, []byte, error) {
			// A bd older than v0.59 refuses --flat; runPlain retries without
			// it, and that retry is a second started subprocess.
			for _, a := range args {
				if a == "--flat" {
					return nil, []byte("unknown flag: --flat"), exitStatusErr(1)
				}
			}
			return []byte("[]"), nil, nil
		}))

		if _, err := b.List(ListOptions{}); err != nil {
			t.Fatalf("List: %v", err)
		}
		if count != 2 {
			t.Errorf("observer ran %d times for a call retried once, want 2", count)
		}
	})
}

// TestSlingCallsNilObserverCountsNothing: a store built without an observer —
// and one given WithCallObserver(nil) — runs its calls unchanged. The nil
// observer is the zero value every wrapper that is not counting keeps, so it
// must neither count nor fail.
func TestSlingCallsNilObserverCountsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		opts   []Option
		broken bool
	}{
		{name: "no observer"},
		{name: "nil observer", opts: []Option{WithCallObserver(nil)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ran := 0
			b := New(t.TempDir(), append(tc.opts, WithRunner(func(context.Context, []string) ([]byte, []byte, error) {
				ran++
				return []byte("[]"), nil, nil
			}))...)
			if b.observe != nil {
				t.Fatal("observe is set, want nil")
			}
			if _, err := b.Comments("gt-abc"); err != nil {
				t.Fatalf("Comments: %v", err)
			}
			if ran == 0 {
				t.Error("bd never ran")
			}
		})
	}
}

// TestSlingCallsObserverSurvivesEveryDerivedWrapper: a store derived from an
// observed one keeps counting. The sling path asks for an agent-scoped or
// per-ID store at each step (ActingAs, ForAgentBead, forIssueID), and a
// derived store that silently dropped the observer would leave that step's
// bd calls off the trace with nothing failing.
func TestSlingCallsObserverSurvivesEveryDerivedWrapper(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	count := 0
	observe := func() { mu.Lock(); count++; mu.Unlock() }
	base := New(t.TempDir(), WithRunner(func(context.Context, []string) ([]byte, []byte, error) {
		return []byte("[]"), nil, nil
	}), WithCallObserver(observe))

	derived := map[string]*Beads{
		"ActingAs":      base.ActingAs("gastown/polecats/ace"),
		"ForAgentBead":  base.ForAgentBead(),
		"pinnedToBeads": base.pinnedToBeadsDir(base.getResolvedBeadsDir()),
		"forIssueID":    base.forIssueID("gt-abc"),
	}
	for name, b := range derived {
		if b.observe == nil {
			t.Errorf("%s dropped the observer", name)
		}
	}
	for name, b := range derived {
		before := count
		if _, err := b.Comments("gt-abc"); err != nil {
			t.Fatalf("%s Comments: %v", name, err)
		}
		if count == before {
			t.Errorf("%s did not count its call", name)
		}
	}
}

// TestSlingCallsObserverSurvivesTimeoutAndProcessGroup: the plain wrapper's
// WithTimeout and WithProcessGroup copies are what a caller reaches for after
// building a plain observed store, and both build a new wrapper.
func TestSlingCallsObserverSurvivesTimeoutAndProcessGroup(t *testing.T) {
	t.Parallel()
	observe := func() {}
	base := NewPlain(t.TempDir(), nil, WithCallObserver(observe))
	for name, b := range map[string]*Beads{
		"WithTimeout":      base.WithTimeout(0),
		"WithProcessGroup": base.WithProcessGroup(),
	} {
		if b.observe == nil {
			t.Errorf("%s dropped the observer", name)
		}
	}
}
