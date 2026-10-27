package store

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// A create's parent is an issue the batch reads and links to, so it is locked up
// front like any other target, unless the same batch creates it.
func TestBatchLockIDsLocksACreatesParent(t *testing.T) {
	t.Parallel()
	ops := []api.BatchOperation{
		{Op: api.BatchOpCreate, Create: &api.CreateParams{Parent: "p-1"}},
	}
	if got, want := batchLockIDs(ops), []string{"p-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("batchLockIDs = %v, want %v", got, want)
	}
}

// A parent the same batch creates is not locked: it is not yet an existing row.
func TestBatchLockIDsSkipsAParentTheBatchCreates(t *testing.T) {
	t.Parallel()
	ops := []api.BatchOperation{
		{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "p-1"}},
		{Op: api.BatchOpCreate, Create: &api.CreateParams{Parent: "p-1"}},
	}
	if got := batchLockIDs(ops); len(got) != 0 {
		t.Errorf("batchLockIDs = %v, want none", got)
	}
}
