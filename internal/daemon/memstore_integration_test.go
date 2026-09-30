//go:build integration

package daemon

import (
	"reflect"
	"testing"
)

// TestIntegrationMemStoreMatchesBeadsStore runs observeStore against a real beads store
// on the package's Dolt container and against memStore, and requires the
// same observation: the differential check behind every convoy test that
// runs on memStore instead of Dolt.
func TestIntegrationMemStoreMatchesBeadsStore(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	real, cleanup := setupTestStore(t)
	defer cleanup()
	mem, memCleanup := newMemStore(t)
	defer memCleanup()

	if got, want := observeStore(t, real), observeStore(t, mem); !reflect.DeepEqual(got, want) {
		t.Errorf("memStore diverges from the beads store:\n beads   %+v\n memStore %+v", got, want)
	}
}
