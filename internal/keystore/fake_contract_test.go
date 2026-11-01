package keystore_test

import (
	"testing"

	"github.com/steveyegge/gastown/internal/keystore"
	"github.com/steveyegge/gastown/internal/keystore/storetest"
)

// TestFakeSatisfiesTheContract runs the shared Store contract over the Fake, so
// the suite every implementation runs is checked against the reference one.
func TestFakeSatisfiesTheContract(t *testing.T) {
	t.Parallel()
	storetest.Run(t, "ws", func(*testing.T) keystore.Store { return keystore.NewFake() })
}
