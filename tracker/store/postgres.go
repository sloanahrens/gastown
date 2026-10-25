package store

import "github.com/jackc/pgx/v5/pgxpool"

// Postgres is the Store over the tracker schema. Its methods live in the
// postgres_*.go files, one per area; contract.RunStoreContract holds them to
// the same behavior as every other Store.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// NewPostgres returns a Store over pool, whose database Migrate has brought
// to SchemaLevel.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }
