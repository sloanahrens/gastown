package cmd

import (
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
