package update

import (
	"context"
	"errors"
	"testing"
)

// TestInstallByDigestLedgerOrder holds the order a roll's safety net depends on:
// the previous pair is on record before the service changes, so a swap that fails
// afterwards can be put back. A record that cannot be written stops the roll
// before the swap; TestInstallByDigestRecordsThePreviousBeforeTheSwap holds that.
func TestInstallByDigestLedgerOrder(t *testing.T) {
	t.Parallel()
	running := ImageRecord{Current: oldPair}

	t.Run("a swap that fails after the record restores the previous pair and keeps it on record", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, running)
		f.swap.fail = map[string]error{newPair.Daemon: errors.New("pod swap refused")}

		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if res.Event != "failed" || res.Rollback == nil || res.Rollback.Restored != oldPair {
			t.Fatalf("result = %+v, want a failed install restored to %+v", res, oldPair)
		}
		if got, want := f.swap.seenAtApply[0], (ImageRecord{Current: newPair, Previous: oldPair}); got != want {
			t.Errorf("the ledger at the failing swap = %+v, want %+v: the previous pair must be recorded first", got, want)
		}
		if f.ledger.rec.Current != oldPair {
			t.Errorf("the ledger after the restore = %+v, want the previous pair as current", f.ledger.rec)
		}
	})
}
