// Package doltargv decides whether a process's argv is a `dolt sql-server`.
//
// It is a leaf package: internal/util (which re-exports the matcher as
// IsDoltSQLServerArgs) imports internal/hoststat through workspace, config
// and townhealth, so hoststat cannot import util to share the matcher.
package doltargv

import "path/filepath"

// IsSQLServer is the one `dolt sql-server` argv matcher: the first token's
// basename is "dolt" and a later token is "sql-server". Global flags between
// the binary and the subcommand (`dolt --data-dir x sql-server`) are allowed:
// externally started servers use them.
//
// It fails closed: a dolt whose argv cannot be read (another uid, a zombie),
// whose binary path contains a space (strings.Fields splits it), or whose
// wrapper renames argv0 is not matched, and callers then refuse to signal it.
// Refusing to stop a genuine wedged dolt is the safe direction. A process that
// merely mentions the words (`tail -f dolt-sql-server.log`) is not matched: no
// token is "sql-server" and argv[0] is not dolt.
func IsSQLServer(args []string) bool {
	if len(args) < 2 || filepath.Base(args[0]) != "dolt" {
		return false
	}
	for _, a := range args[1:] {
		if a == "sql-server" {
			return true
		}
	}
	return false
}
