//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m, func(ctx context.Context, c *pgtest.Cluster) error {
		return c.Template(ctx, "empty", func(context.Context, *pgxpool.Pool) error { return nil })
	}))
}
