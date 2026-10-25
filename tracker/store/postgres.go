package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
)

// Postgres is the Store over the tracker schema. Its methods land one bead
// at a time (plan tasks V2 onward); until then each returns ErrUnimplemented
// naming itself, so a half-built server fails loudly instead of answering
// with an empty result.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// NewPostgres returns a Store over pool, whose database Migrate has brought
// to SchemaLevel.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func unimplemented(method string) error {
	return fmt.Errorf("%w: Postgres.%s", ErrUnimplemented, method)
}

func (*Postgres) UpdateIssue(context.Context, string, api.UpdatePatch, api.Guard) error {
	return unimplemented("UpdateIssue")
}
func (*Postgres) CloseIssues(context.Context, []string, string, bool) error {
	return unimplemented("CloseIssues")
}
func (*Postgres) DeleteIssues(context.Context, []string) error { return unimplemented("DeleteIssues") }
func (*Postgres) AddComment(context.Context, string, string, string) error {
	return unimplemented("AddComment")
}
func (*Postgres) Comments(context.Context, string) ([]api.Comment, error) {
	return nil, unimplemented("Comments")
}
func (*Postgres) AddDependency(context.Context, string, string, string) error {
	return unimplemented("AddDependency")
}
func (*Postgres) RemoveDependency(context.Context, string, string) error {
	return unimplemented("RemoveDependency")
}
func (*Postgres) Dependencies(context.Context, string, string) ([]api.IssueDep, error) {
	return nil, unimplemented("Dependencies")
}
func (*Postgres) Ready(context.Context, api.ReadyFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("Ready")
}
func (*Postgres) Blocked(context.Context, api.ReadyFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("Blocked")
}
func (*Postgres) ChildrenOf(context.Context, []string) (map[string][]api.Issue, error) {
	return nil, unimplemented("ChildrenOf")
}
func (*Postgres) EventsAfter(context.Context, int64, int) (api.EventsPage, error) {
	return api.EventsPage{}, unimplemented("EventsAfter")
}
