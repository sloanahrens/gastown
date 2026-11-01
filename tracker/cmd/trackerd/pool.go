package main

import (
	"errors"
	"net/url"
	"strings"

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
// It must look before pgxpool.ParseConfig, which removes the pool_* keys.
func dsnSetsPoolMaxConns(dsn string) (bool, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return false, err
		}
		return u.Query().Has("pool_max_conns"), nil
	}
	for _, key := range keywordKeys(dsn) {
		if key == "pool_max_conns" {
			return true, nil
		}
	}
	return false, nil
}

// keywordKeys lists the keywords of a key=value connection string. A value may
// be single-quoted, with backslash escapes, so a quoted value that contains
// "pool_max_conns=" is not read as a keyword.
func keywordKeys(dsn string) []string {
	var keys []string
	i := 0
	for i < len(dsn) {
		if isSpace(dsn[i]) {
			i++
			continue
		}
		start := i
		for i < len(dsn) && dsn[i] != '=' && !isSpace(dsn[i]) {
			i++
		}
		keys = append(keys, dsn[start:i])
		for i < len(dsn) && isSpace(dsn[i]) {
			i++
		}
		if i < len(dsn) && dsn[i] == '=' {
			i++
		}
		for i < len(dsn) && isSpace(dsn[i]) {
			i++
		}
		if i < len(dsn) && dsn[i] == '\'' {
			i++
			for i < len(dsn) && dsn[i] != '\'' {
				if dsn[i] == '\\' {
					i++
				}
				i++
			}
			i++
			continue
		}
		for i < len(dsn) && !isSpace(dsn[i]) {
			i++
		}
	}
	return keys
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
