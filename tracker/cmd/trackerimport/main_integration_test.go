//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// Templates: "migrated" has the tracker's schema applied, which the import
// writes into.
func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m, func(ctx context.Context, c *pgtest.Cluster) error {
		return c.Template(ctx, "migrated", store.Migrate)
	}))
}
