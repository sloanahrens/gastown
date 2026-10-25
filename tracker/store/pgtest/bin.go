// Package pgtest starts a private PostgreSQL 17 for tests: one cluster per
// test package on a unix socket in a temp directory, one template database
// built once, and a cheap clone of it per test. It never listens on TCP.
//
// Locating the binaries is untagged and unit-tested; the cluster itself needs
// the real binaries and builds only with the integration tag.
package pgtest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// binEnv overrides where the PostgreSQL binaries are found. It names the
// directory holding postgres and initdb.
const binEnv = "TRACKER_PG_BIN"

// knownDirs are where a PostgreSQL 17 install puts its binaries when PATH
// does not: the Homebrew keg on Apple silicon and Intel, and Debian's layout.
var knownDirs = []string{
	"/opt/homebrew/opt/postgresql@17/bin",
	"/usr/local/opt/postgresql@17/bin",
	"/usr/lib/postgresql/17/bin",
}

// Locate returns the directory holding postgres and initdb: $TRACKER_PG_BIN,
// else where postgres is on PATH, else a known install directory. The lookups
// are parameters so a unit test can supply them.
func Locate(getenv func(string) string, exists func(string) bool, lookPath func(string) (string, error)) (string, error) {
	var candidates []string
	if d := getenv(binEnv); d != "" {
		candidates = append(candidates, d)
	}
	if p, err := lookPath("postgres"); err == nil {
		candidates = append(candidates, filepath.Dir(p))
	}
	candidates = append(candidates, knownDirs...)
	for _, dir := range candidates {
		if exists(filepath.Join(dir, "postgres")) {
			if !exists(filepath.Join(dir, "initdb")) {
				return "", fmt.Errorf("pgtest: %s has postgres but no initdb", dir)
			}
			return dir, nil
		}
	}
	return "", fmt.Errorf("pgtest: no PostgreSQL 17 binaries found; install them (brew install postgresql@17) or set %s to the directory holding postgres and initdb", binEnv)
}

var versionRE = regexp.MustCompile(`\(PostgreSQL\) (\d+)(?:\.|\b)`)

// requireMajor17 checks the output of `postgres --version` names major 17,
// the version the design pins for parity with the town's other Postgres.
func requireMajor17(out string) error {
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("pgtest: cannot read a PostgreSQL version from %q", strings.TrimSpace(out))
	}
	if m[1] != "17" {
		return fmt.Errorf("pgtest: PostgreSQL %s found, want 17", m[1])
	}
	return nil
}
