//go:build integration

package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// Integration tier: concurrent callers must get distinct child numbers.
func TestChildIDsAreDistinctUnderConcurrency(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	ctx := context.Background()
	const n = 20
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			id, err := nextChildID(ctx, tx, "gt-parent")
			if err != nil {
				_ = tx.Rollback(ctx)
				errs[i] = err
				return
			}
			ids[i], errs[i] = id, tx.Commit(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	sort.Slice(ids, func(a, b int) bool {
		return len(ids[a]) < len(ids[b]) || (len(ids[a]) == len(ids[b]) && ids[a] < ids[b])
	})
	for i, id := range ids {
		if want := fmt.Sprintf("gt-parent.%d", i+1); id != want {
			t.Errorf("child %d = %q, want %q (distinct, gapless)", i, id, want)
		}
	}
}

func TestChildNumbersDoNotCollideAcrossParents(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	ctx := context.Background()
	next := func(parent string) string {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := nextChildID(ctx, tx, parent)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if a, b, a2 := next("gt-a"), next("gt-b"), next("gt-a"); a != "gt-a.1" || b != "gt-b.1" || a2 != "gt-a.2" {
		t.Errorf("got %s %s %s, want gt-a.1 gt-b.1 gt-a.2", a, b, a2)
	}
}
