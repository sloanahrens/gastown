package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// Stats counts the durable issues of rig, or of every rig when rig is empty, by
// status, plus its wisps as one total. Closed issues are counted; a wisp's
// status is not broken out, so the two counts never mix.
func (p *Postgres) Stats(ctx context.Context, rig string) (api.Stats, error) {
	rows, err := p.pool.Query(ctx, "SELECT status, count(*) FILTER (WHERE NOT ephemeral), count(*) FILTER (WHERE ephemeral) FROM tracker.issues WHERE ($1 = '' OR rig = $1) GROUP BY status", rig)
	if err != nil {
		return api.Stats{}, fmt.Errorf("store: stats: %w", err)
	}
	defer rows.Close()
	out := api.Stats{ByStatus: map[string]int{}}
	for rows.Next() {
		var status string
		var durable, wisps int
		if err := rows.Scan(&status, &durable, &wisps); err != nil {
			return api.Stats{}, fmt.Errorf("store: stats: %w", err)
		}
		// A status held by wisps alone is not a durable status, so it is left
		// out of ByStatus rather than reported as zero.
		if durable > 0 {
			out.ByStatus[status] = durable
			out.Total += durable
		}
		out.Wisps += wisps
	}
	return out, rows.Err()
}

// The configuration lives in tracker.config, created by schema/0001_core.sql; the
// config contract case runs against a freshly migrated database and so proves it.

// ConfigGet returns the value of a configuration key; ok is false when unset.
func (p *Postgres) ConfigGet(ctx context.Context, key string) (string, bool, error) {
	if key == "" {
		return "", false, api.Invalid("a configuration key is required")
	}
	var v string
	err := p.pool.QueryRow(ctx, "SELECT value FROM tracker.config WHERE key = $1", key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: config get: %w", err)
	}
	return v, true, nil
}

// ConfigSet sets a configuration key, replacing any earlier value.
func (p *Postgres) ConfigSet(ctx context.Context, key, value string) error {
	if key == "" {
		return api.Invalid("a configuration key is required")
	}
	if _, err := p.pool.Exec(ctx, "INSERT INTO tracker.config (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", key, value); err != nil {
		return fmt.Errorf("store: config set: %w", err)
	}
	return nil
}
