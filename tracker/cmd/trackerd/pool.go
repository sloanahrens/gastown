package main

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultPoolMaxConns caps the pool when the DSN does not set pool_max_conns.
// pgxpool's own default, the larger of 4 and the CPU count, is not tied to the
// Postgres server's max_connections.
const defaultPoolMaxConns = 30

// errBadDSN never carries the DSN: a parse error from pgx can echo the password.
var errBadDSN = errors.New("cannot use TRACKER_DSN as a connection string")

// poolConfig reads dsn into a pgxpool config and bounds the pool at
// defaultPoolMaxConns unless the DSN itself sets pool_max_conns.
func poolConfig(dsn string) (*pgxpool.Config, error) {
	setsMax, err := dsnSetsPoolMaxConns(dsn)
	if err != nil {
		return nil, errBadDSN
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errBadDSN
	}
	if !setsMax {
		cfg.MaxConns = defaultPoolMaxConns
	}
	return cfg, nil
}

// dsnSetsPoolMaxConns reports whether dsn names pool_max_conns as a parameter.
// It asks pgconn, the parser pgxpool itself uses, so the answer cannot diverge
// from what pgxpool reads.
func dsnSetsPoolMaxConns(dsn string) (bool, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return false, err
	}
	_, set := cfg.RuntimeParams["pool_max_conns"]
	return set, nil
}
