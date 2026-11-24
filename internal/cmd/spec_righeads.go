package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/config"
)

// rigDatabases maps each named rig to the Dolt database its own bd
// .beads/metadata.json records. A rig whose metadata names no readable
// database — absent, unparseable or empty — is left out, so a caller asks for
// no head it cannot address. The name is read from the file, never derived
// from the rig name (gastown is gt, beads is be).
func rigDatabases(townRoot string, rigs []string) map[string]string {
	databases := make(map[string]string, len(rigs))
	for _, rig := range rigs {
		if db, _ := config.RigMetadataDatabase(townRoot, rig); db != "" {
			databases[rig] = db
		}
	}
	return databases
}

// rigHeadsQuery is the one statement that asks every rig database for its head
// and working hash, naming the databases in the order given. It takes the
// statement out of the read so the text is fixed without a store; rigHeads
// sends it (gt-ihvt1v).
func rigHeadsQuery(databases []string) beadsql.Query {
	return beadsql.RigHeads(databases)
}

// rigHashRow is one database's hashes: the database beadsql.ParseRigHeads
// paired the head and working columns to, so the rig keying the next bead of
// the chain can find a rig's hashes through the database it uses.
type rigHashRow struct {
	Database string
	Head     string
	Working  string
}

// parseRigHeads parses one SQLCSV result — the header record and the one row
// RigHeads answers — with beadsql.ParseRigHeads, which pairs a database's head
// with its working hash by column name rather than position. Each database
// becomes one row, sorted by database so two runs read the same. A result that
// is not a header and one row is refused: reading the first of several rows
// would pass a malformed answer off as one database per rig (gt-5v8y37).
func parseRigHeads(records [][]string) ([]rigHashRow, error) {
	if len(records) != 2 {
		return nil, fmt.Errorf("rigHeads: %d SQLCSV records, want a header and one row", len(records))
	}
	parsed, err := beadsql.ParseRigHeads(records[0], records[1])
	if err != nil {
		return nil, err
	}
	rows := make([]rigHashRow, 0, len(parsed))
	for database, hash := range parsed {
		rows = append(rows, rigHashRow{Database: database, Head: hash.Head, Working: hash.Working})
	}
	slices.SortFunc(rows, func(a, b rigHashRow) int { return strings.Compare(a.Database, b.Database) })
	return rows, nil
}

// rigHeads asks every named rig's database for its two hashes in one SQLCSV
// round trip, so a rig costs no call of its own, and parses the answer with
// ParseRigHeads into one row per database. The databases are the ones
// rigDatabases names, sorted so two runs send the same statement. A rig whose
// metadata names no database is not asked for, and a set with none refuses
// rather than answering empty (gt-ihvt1v, gt-5v8y37).
func rigHeads(townRoot string, rigs []string, read sqlReader) ([]rigHashRow, error) {
	databases := rigDatabases(townRoot, rigs)
	names := make([]string, 0, len(databases))
	for _, db := range databases {
		names = append(names, db)
	}
	slices.Sort(names)
	records, err := read.SQLCSV(rigHeadsQuery(names))
	if err != nil {
		return nil, err
	}
	return parseRigHeads(records)
}
