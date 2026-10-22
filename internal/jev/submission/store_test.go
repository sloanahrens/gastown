package submission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/battery"
)

// countingCloner records the fetches and reads that reach the shared clone.
type countingCloner struct {
	synced int
	ran    int
}

func (c *countingCloner) Sync(context.Context) error {
	c.synced++
	return nil
}

func (c *countingCloner) Run(context.Context, ...string) (string, error) {
	c.ran++
	return "", nil
}

// FetchLocked's fetch waits for the clone's fetch lock: with another fetch in
// flight it waits, and the wait ends with the ctx without reaching the clone.
// Once the holder finishes, the fetch goes ahead and releases the lock.
// Reads take no lock, so Run goes ahead while the lock is held.
func TestFetchLockedSyncWaitsForTheCloneLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clone := &countingCloner{}
	f := FetchLocked{Cloner: clone, TownRoot: town}

	hold, err := battery.Lock(FetchLockPath(town))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := f.Sync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync with the clone locked = %v, want the wait to end with the ctx", err)
	}
	if clone.synced != 0 {
		t.Errorf("the clone was fetched while another fetch held it")
	}
	if _, err := f.Run(context.Background(), "status"); err != nil {
		t.Fatal(err)
	}
	if clone.ran != 1 {
		t.Errorf("ran %d times while the lock was held, want the read to go ahead", clone.ran)
	}

	hold()
	if err := f.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if clone.synced != 1 {
		t.Errorf("synced %d times, want the one fetch after the holder finished", clone.synced)
	}
	again, err := battery.Lock(FetchLockPath(town))
	if err != nil {
		t.Fatalf("fetch left the clone's fetch lock held: %v", err)
	}
	again()
}
