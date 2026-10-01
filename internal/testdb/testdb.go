// Package testdb owns the one list of database-name prefixes that mark a
// Dolt database as test cruft (deep review B2-03, gt-fcxe9.9).
//
// Test code mints these names; orphan cleanup, the reaper, the JSONL backup
// and gt dolt's migrate walk skip or remove them. The list used to live in
// seven copies that disagreed. Every Go caller in this module imports this
// package; TestOneDefinition fails on a new copy.
//
// It is a leaf package with no gastown imports, so anything can use it.
package testdb

import "strings"

// MintPrefix starts the throwaway database names isolated test inits and
// testutil's Dolt pool create ("testdb_" + a hash or random hex). bd's own
// test-database firewall recognizes it too.
const MintPrefix = "testdb_"

// RemotesCheckPrefix starts the plain SQL databases daemon remote tests and
// testutil's SQL pool create.
const RemotesCheckPrefix = "dolt_remotes_check_"

// prefixes is the union of every list that existed: gastown's copies plus the
// beads fork's (beads_vr, doctortest_, benchdb_). "beads_t" covers bd's
// "beads_test". Matching is case-insensitive.
var prefixes = []string{
	MintPrefix,
	"beads_t",
	"beads_pt",
	"beads_vr",
	"doctest_",
	"doctortest_",
	"benchdb_",
	RemotesCheckPrefix,
}

// Prefixes returns a copy of the test-database prefixes, in a fixed order.
func Prefixes() []string {
	return append([]string(nil), prefixes...)
}

// IsTestDatabaseName reports whether name carries a test-database prefix.
func IsTestDatabaseName(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
