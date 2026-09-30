package util

import "path/filepath"

// IsDoltSQLServerArgs is the one `dolt sql-server` argv matcher: the first
// token's basename is "dolt" and a later token is "sql-server". Global flags
// between the binary and the subcommand (`dolt --data-dir x sql-server`) are
// allowed: externally started servers use them.
//
// It fails closed: a dolt whose argv cannot be read (another uid, a zombie),
// whose binary path contains a space (strings.Fields splits it), or whose
// wrapper renames argv0 is not matched, and callers then refuse to signal it.
// Refusing to stop a genuine wedged dolt is the safe direction.
//
// No build tag: doltserver's pre-signal identity check needs it on every
// platform, and the orphan scan in dolt_orphan.go (non-Windows) calls it too.
func IsDoltSQLServerArgs(args []string) bool {
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
