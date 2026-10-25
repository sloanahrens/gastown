//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/steveyegge/gastown/tracker/contract"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// Integration tier: every registered contract case against the real Postgres
// store, one migrated clone per case.
func TestPostgresSatisfiesTheStoreContract(t *testing.T) {
	contract.RunStoreContract(t, func(t *testing.T) store.Store {
		pool := pgtest.New(t, "migrated")
		if _, err := pool.Exec(context.Background(), "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')"); err != nil {
			t.Fatal(err)
		}
		return store.NewPostgres(pool)
	})
}
