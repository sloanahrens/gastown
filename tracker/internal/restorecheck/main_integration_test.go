//go:build integration

package restorecheck

import (
	"context"
	"os"
	"testing"

	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// The package's cluster, with one template carrying the tracker schema; each
// test clones it, so no test sees another's rows.
func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m, func(ctx context.Context, c *pgtest.Cluster) error {
		return c.Template(ctx, "migrated", store.Migrate)
	}))
}
