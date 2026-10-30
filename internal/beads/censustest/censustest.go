// Package censustest asserts how many bd calls a function makes, so a cut to a
// busy caller cannot quietly regress (gt-gojo0). FakeBD answers the calls
// in-process, which is the fake bd the census counts.
package censustest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// Answer replies to one bd call with the stdout bd would print.
type Answer func(argv []string) (stdout []byte, err error)

// Recorder holds the argv of every bd call a FakeBD answered.
type Recorder struct {
	mu    sync.Mutex
	argvs []string
}

// Argvs returns each answered call's argv joined by spaces, oldest first.
func (r *Recorder) Argvs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.argvs...)
}

func (r *Recorder) record(argv []string) {
	r.mu.Lock()
	r.argvs = append(r.argvs, strings.Join(argv, " "))
	r.mu.Unlock()
}

// FakeBD returns a store whose bd calls answer answers instead of starting a
// bd binary, and the recorder that saw their argv. A nil answer replies "[]",
// which every list-shaped read accepts. The store answers bd's --allow-stale
// capability probe as a bd too old for the flag: the probe reaches neither the
// recorder nor the census, so both count the caller's own calls (gt-gojo0).
func FakeBD(dir string, answer Answer) (*beads.Beads, *Recorder) {
	rec := &Recorder{}
	store := beads.New(dir, beads.WithRunner(func(_ context.Context, argv []string) ([]byte, []byte, error) {
		if len(argv) == 2 && argv[0] == "--allow-stale" && argv[1] == "version" {
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		}
		rec.record(argv)
		if answer == nil {
			return []byte("[]"), nil, nil
		}
		out, err := answer(argv)
		return out, nil, err
	}))
	return store, rec
}

// CheckBudget runs fn and returns an error naming the count and the budget
// when fn started more bd calls than budget. The census counts bd subprocesses
// process-wide, so a caller running this must not share the process with a
// parallel test's calls.
func CheckBudget(budget int, fn func()) error {
	beads.CensusSnapshotAndReset()
	defer beads.CensusSnapshotAndReset()
	fn()
	if calls := beads.CensusTotal(); calls > budget {
		return fmt.Errorf("started %d bd call(s), budget %d", calls, budget)
	}
	return nil
}

// AssertBudget fails tb when fn started more than budget bd calls.
func AssertBudget(tb testing.TB, budget int, fn func()) {
	tb.Helper()
	if err := CheckBudget(budget, fn); err != nil {
		tb.Errorf("bd call budget: %v", err)
	}
}
