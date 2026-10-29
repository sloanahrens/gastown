package restorecheck

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// Reader is the read side of a tracker database: the three reads the check
// makes. store.Store satisfies it, and a test supplies its own.
type Reader interface {
	Stats(ctx context.Context, rig string) (api.Stats, error)
	ListIssues(ctx context.Context, f api.ListFilter) (api.IssuePage, error)
	GetIssues(ctx context.Context, ids []string) (map[string]api.Issue, error)
}

var _ Reader = (*store.Postgres)(nil)

// ReadOnlySubset reads one count, one page of the default list and one issue
// by id from a restored copy, and reports the first read that comes back empty
// or disagrees with the others. It writes nothing.
func ReadOnlySubset(ctx context.Context, rd Reader) error {
	stats, err := rd.Stats(ctx, "")
	if err != nil {
		return fmt.Errorf("count the issues: %w", err)
	}
	if stats.Total == 0 {
		return errors.New("the restored copy holds no issue")
	}
	page, err := rd.ListIssues(ctx, api.ListFilter{Limit: 1})
	if err != nil {
		return fmt.Errorf("list the issues: %w", err)
	}
	if len(page.Issues) == 0 {
		return errors.New("the restored copy lists no issue")
	}
	first := page.Issues[0]
	got, err := rd.GetIssues(ctx, []string{first.ID})
	if err != nil {
		return fmt.Errorf("read issue %s by id: %w", first.ID, err)
	}
	shown, ok := got[first.ID]
	if !ok {
		return fmt.Errorf("issue %s is in the list but does not read back by id", first.ID)
	}
	if shown.Title != first.Title || shown.Status != first.Status {
		return fmt.Errorf("issue %s reads back with a different title or status", first.ID)
	}
	return nil
}

// readSubset is the default Read: a pool on the scratch database, read through
// the store's contract.
func readSubset(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	return ReadOnlySubset(ctx, store.NewPostgres(pool))
}
