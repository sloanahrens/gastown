package cmd

import (
	"fmt"
	"regexp"
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
// ParseRigHeads into one row per database. Each rig is then keyed by its own
// name with the hashes of the database rigDatabases names for it, so a caller
// holding a rig asks nothing about databases. The databases are sorted so two
// runs send the same statement. A rig whose metadata names no database is not
// asked for and is keyed nowhere, and a set with none refuses rather than
// answering empty (gt-j3vdg4, gt-ihvt1v, gt-5v8y37).
//
// A database the server does not know — a stale metadata file names one the
// server has dropped — costs only its own rig its hashes: it is dropped by
// exact name and the statement sent once more without it, so its rig is absent
// and every other rig is answered. When no database is left to ask, the answer
// holds no hashes rather than a statement Dolt would refuse (gt-a8ak44).
func rigHeads(townRoot string, rigs []string, read sqlReader) (map[string]beadsql.RigHash, error) {
	databases := rigDatabases(townRoot, rigs)
	names := make([]string, 0, len(databases))
	for _, db := range databases {
		names = append(names, db)
	}
	slices.Sort(names)
	records, err := read.SQLCSV(rigHeadsQuery(names))
	if err != nil {
		dropped, ok := unknownDatabase(err, names)
		if !ok {
			return nil, err
		}
		names = slices.DeleteFunc(names, func(db string) bool { return db == dropped })
		if len(names) == 0 {
			return map[string]beadsql.RigHash{}, nil
		}
		if records, err = read.SQLCSV(rigHeadsQuery(names)); err != nil {
			return nil, err
		}
	}
	rows, err := parseRigHeads(records)
	if err != nil {
		return nil, err
	}
	byDatabase := make(map[string]beadsql.RigHash, len(rows))
	for _, row := range rows {
		byDatabase[row.Database] = beadsql.RigHash{Head: row.Head, Working: row.Working}
	}
	heads := make(map[string]beadsql.RigHash, len(rigs))
	for rig, db := range databases {
		if hash, ok := byDatabase[db]; ok {
			heads[rig] = hash
		}
	}
	return heads, nil
}

// unknownSystemVariable finds the database an unknown-variable refusal names in
// its '@@<database>_head' or '@@<database>_working' variable. The suffix has to
// end the name (\b), so a variable that only starts with one of the statement's
// own — @@om_head_ref — does not read as it.
var unknownSystemVariable = regexp.MustCompile(`(?i)@@([A-Za-z0-9_-]+)_(?:head|working)\b`)

// unknownDatabase names the database a failed read refused because the server
// does not know it. Dolt answers an unknown database's head variable with error
// 1105, "Unknown system variable '@@<database>_head'", which is what a stale
// metadata file naming a dropped database gets. The refusal's words are matched
// case-insensitively, and the name it gives is matched exactly against the
// databases the statement asked for: a name that only starts with one of them
// (om for om2) is no match. A failure that is not that refusal, or that names
// no database asked for, is no unknown database, and the read's error stands.
func unknownDatabase(err error, databases []string) (string, bool) {
	text := err.Error()
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "error 1105") || !strings.Contains(lower, "unknown system variable") {
		return "", false
	}
	for _, match := range unknownSystemVariable.FindAllStringSubmatch(text, -1) {
		if slices.Contains(databases, match[1]) {
			return match[1], true
		}
	}
	return "", false
}
