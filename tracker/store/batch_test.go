package store

import (
	"context"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// The guard keeps an op kind that api gains without a case here from running
// as a no-op write. The nil transaction is never reached: the guard returns
// first, and a case added above it turns this test into a loud panic.
func TestBatchDispatchRefusesAnOpNoCaseHandles(t *testing.T) {
	t.Parallel()
	if _, err := batchOpInTx(context.Background(), nil, api.BatchOperation{Op: "delete"}, "town/crew/x"); err == nil {
		t.Error("batchOpInTx accepted an op kind with no case")
	}
}
