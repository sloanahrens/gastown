package main

import (
	"strings"
	"testing"
)

// The pool is bounded at 30 when the DSN does not set pool_max_conns, in both
// URL and keyword form. A name that only looks like the parameter does not count.
func TestPoolConfigDefaultsToThirty(t *testing.T) {
	t.Parallel()
	for name, dsn := range map[string]string{
		"url":                        "postgres://u:p@h:5432/db",
		"url scheme alias":           "postgresql://u:p@h:5432/db",
		"keyword":                    "host=h port=5432 dbname=db",
		"name in database path":      "postgres://u:p@h/pool_max_conns=7",
		"name inside a quoted value": "host=h password='pool_max_conns=7' dbname=db",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := poolConfig(dsn)
			if err != nil {
				t.Fatalf("poolConfig(%s) error: %v", name, err)
			}
			if cfg.MaxConns != 30 {
				t.Errorf("MaxConns = %d, want 30", cfg.MaxConns)
			}
		})
	}
}

// A DSN that sets pool_max_conns keeps its value.
func TestPoolConfigKeepsTheDSNValue(t *testing.T) {
	t.Parallel()
	for name, dsn := range map[string]string{
		"url":     "postgres://u:p@h/db?pool_max_conns=7",
		"keyword": "host=h pool_max_conns=7 dbname=db",
		// An escaped space stays inside an unquoted value, so pool_max_conns after it is still a keyword.
		"keyword after an escaped space": `host=h password=a\ b pool_max_conns=7`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := poolConfig(dsn)
			if err != nil {
				t.Fatalf("poolConfig(%s) error: %v", name, err)
			}
			if cfg.MaxConns != 7 {
				t.Errorf("MaxConns = %d, want 7", cfg.MaxConns)
			}
		})
	}
}

// An unreadable DSN is refused with the fixed message, and the message never
// carries the DSN (its password).
func TestPoolConfigNeverEchoesTheDSN(t *testing.T) {
	t.Parallel()
	const want = "cannot use TRACKER_DSN as a connection string"
	for name, dsn := range map[string]string{
		"url with a bad port":          "postgres://u:hunter2@h:notaport/db",
		"keyword with a bad port":      "host=h port=notaport password=hunter2",
		"keyword with a bad pool size": "host=h password=hunter2 pool_max_conns=abc",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := poolConfig(dsn)
			if err == nil {
				t.Fatalf("poolConfig(%s) = %+v, want an error", name, cfg)
			}
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error leaks the DSN password: %q", err.Error())
			}
		})
	}
}
