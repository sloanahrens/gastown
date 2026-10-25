// Package contract holds every store.Store implementation to the same
// behavior. Cases register themselves from the file of the verb they pin
// (cases_<verb>.go), so verb beads never edit the same file. Postgres runs
// every case; the in-memory fake runs only the ones flagged Fake, because it
// models what handler unit tests need and nothing more.
package contract

import (
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/tracker/store"
)

// Case is one behavior of a store.
type Case struct {
	// Name is unique across all registered cases.
	Name string
	// Fake marks a case the in-memory fake must also pass.
	Fake bool
	// Run asserts the behavior on a fresh, empty store in which the rigs gt
	// and hq exist.
	Run func(t *testing.T, s store.Store)
}

// caseSet is an ordered set of cases with unique names.
type caseSet struct{ cases []Case }

// add appends c. A duplicate name is a programming error and panics.
func (s *caseSet) add(c Case) {
	for _, have := range s.cases {
		if have.Name == c.Name {
			panic(fmt.Sprintf("contract: case %q registered twice", c.Name))
		}
	}
	s.cases = append(s.cases, c)
}

var registry caseSet

// register adds c to the package registry; it is called from init() in a
// cases_*.go file.
func register(c Case) { registry.add(c) }

// RunStoreContract runs every case against stores from newStore (Postgres).
// newStore returns a fresh, empty store in which the rigs gt and hq exist.
func RunStoreContract(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	run(t, newStore, func(Case) bool { return true })
}

// RunFakeContract runs the cases flagged Fake: the subset the in-memory fake
// implements. Each such case must also be in RunStoreContract's set.
func RunFakeContract(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	run(t, newStore, func(c Case) bool { return c.Fake })
}

func run(t *testing.T, newStore func(t *testing.T) store.Store, want func(Case) bool) {
	t.Helper()
	for _, c := range registry.cases {
		if !want(c) {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, newStore(t))
		})
	}
}

// Names returns the registered case names, for tests that pin the set.
func Names() []string {
	out := make([]string, len(registry.cases))
	for i, c := range registry.cases {
		out[i] = c.Name
	}
	return out
}
