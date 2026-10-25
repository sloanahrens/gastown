//go:build integration

package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// Templates: "empty" has nothing; "migrated" has the binary's schema applied.
func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m, func(ctx context.Context, c *pgtest.Cluster) error {
		if err := c.Template(ctx, "empty", func(context.Context, *pgxpool.Pool) error { return nil }); err != nil {
			return err
		}
		return c.Template(ctx, "migrated", Migrate)
	}))
}
