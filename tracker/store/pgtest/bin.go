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

// childEnv is the environment postgres and initdb run with: parent's, with
// every locale variable replaced by the C locale. A daemon under launchd has
// no locale, and postgres on macOS then dies at startup ("postmaster became
// multithreaded during startup ... Set the LC_ALL environment variable to a
// valid locale"); a shell has one, which hides it. The cluster is created with
// --locale=C, so C is also what it must run under.
func childEnv(parent []string) []string {
	out := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		if name == "LANG" || name == "LANGUAGE" || name == "LC_ALL" || strings.HasPrefix(name, "LC_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LC_ALL=C", "LANG=C")
}
