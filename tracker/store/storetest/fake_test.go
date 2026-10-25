package storetest

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/tracker/contract"
	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
	"github.com/steveyegge/gastown/tracker/store"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }

// The fake is held to the same contract as Postgres. Unit tier: in-memory,
// microseconds per case.
func TestFakeSatisfiesTheStoreContract(t *testing.T) {
	contract.RunStoreContract(t, func(*testing.T) store.Store { return NewFake() })
}
