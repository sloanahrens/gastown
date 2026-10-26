//go:build integration

package server

import (
	"context"
	"os"
	"testing"

	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// TestMain starts one throwaway Postgres for the package; each integration
// case clones a migrated template from it (about 70 ms), so no case pays for
// initdb.
func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m, func(ctx context.Context, c *pgtest.Cluster) error {
		return c.Template(ctx, "migrated", store.Migrate)
	}))
}
