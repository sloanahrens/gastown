package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	gtevents "github.com/steveyegge/gastown/internal/events"
)

// sqlRefusalText is the bd gateway's HTTP 403 refusal of a bd sql read, worded
// as gtwrap produces it, so beads reads it as a refusal to remember.
const sqlRefusalText = `gtwrap: the bd gateway at http://127.0.0.1:7777 refused the call with HTTP 403: forbidden: read token may not run sql (write)`

// sqlRefusingBD refuses every bd sql call the way a read-token gateway does,
// answers the wisp list, and answers other calls with an empty list. It counts
// the sql calls that reached it. The poll runs no goroutines, so no lock.
type sqlRefusingBD struct{ sqlCalls int }

func (b *sqlRefusingBD) run(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
	// beads may prepend --allow-stale, so the verb is not always first.
	if slices.Contains(args, "sql") {
		b.sqlCalls++
		return nil, []byte(sqlRefusalText), errors.New(sqlRefusalText)
	}
	if slices.Contains(args, "wisp") && slices.Contains(args, "list") {
		return []byte(`{"wisps": [], "count": 0}`), nil, nil
	}
	return []byte("[]"), nil, nil
}

// dashSeatPoll runs one dashSeats poll through cache. The clock first moves
// past the cache's ttl, so the seat is re-classified and its merge-request
// read runs again.
func dashSeatPoll(t *testing.T, townRoot string, cache *dashSeatCache, clock *time.Time, bd *sqlRefusingBD) {
	t.Helper()
	*clock = clock.Add(cache.ttl + time.Minute)
	cache.storeOpts = []beads.Option{beads.WithDirRunner(bd.run)}
	if _, err := dashSeats(townRoot, nil, true, cache, gtevents.NewDispatchReader(townRoot), fakeSessionLister{}, nil); err != nil {
		t.Fatalf("dashSeats: %v", err)
	}
}

// dashSeatRigTown lays out a town with one rig and one polecat, so a poll
// reaches the seat's merge-request read.
func dashSeatRigTown(t *testing.T) string {
	t.Helper()
	rigPath := routeRigTown(t, beads.BackendTracker)
	seatBuildWorktree(t, rigPath)
	townRoot := filepath.Dir(rigPath)
	writeTestFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), `{"version":1,"rigs":{"gastown":{}}}`)
	return townRoot
}

// TestDashboardSQLRefusal: the refused bd sql read is sent once per cache, not
// once per poll, and each cache keeps a memory of its own.
func TestDashboardSQLRefusal(t *testing.T) {
	t.Parallel()
	t.Run("one cache sends the refused sql once across polls", func(t *testing.T) {
		t.Parallel()
		clock := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
		townRoot := dashSeatRigTown(t)
		cache := newDashSeatCache()
		cache.now = func() time.Time { return clock }
		bd := &sqlRefusingBD{}
		dashSeatPoll(t, townRoot, cache, &clock, bd)
		dashSeatPoll(t, townRoot, cache, &clock, bd)
		if bd.sqlCalls != 1 {
			t.Fatalf("bd sql calls over two polls = %d, want 1", bd.sqlCalls)
		}
	})
	t.Run("two caches each send the refused sql once", func(t *testing.T) {
		t.Parallel()
		clock := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
		townRoot := dashSeatRigTown(t)
		bd := &sqlRefusingBD{}
		fresh := func() *dashSeatCache {
			cache := newDashSeatCache()
			cache.now = func() time.Time { return clock }
			return cache
		}
		dashSeatPoll(t, townRoot, fresh(), &clock, bd)
		dashSeatPoll(t, townRoot, fresh(), &clock, bd)
		if bd.sqlCalls != 2 {
			t.Fatalf("bd sql calls across two caches = %d, want 2 (one per cache)", bd.sqlCalls)
		}
	})
}
