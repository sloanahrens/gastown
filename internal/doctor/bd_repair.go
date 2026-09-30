package doctor

import "github.com/steveyegge/gastown/internal/beads"

// bdRepairer is the bd write surface doctor fixers repair beads through
// (gt-fcxe9.12, ADR 0001): typed bd verbs in machine mode, never a SQL write
// against a bd table, so bd keeps is_blocked and the events journal right.
// A *beads.Beads provides it.
type bdRepairer interface {
	// DemoteToWisp moves an ephemeral-flagged row from issues to wisps
	// ("bd update <id> --ephemeral").
	DemoteToWisp(id string) error
	// ReopenUnassigned returns an unclaimed in_progress row to open
	// ("bd update <id> --status=open --assignee=").
	ReopenUnassigned(id string) error
	// Update applies opts to id in this database only.
	Update(id string, opts beads.UpdateOptions) error
}

// bdRepairOpener returns the bdRepairer pinned to dir's database.
type bdRepairOpener func(dir string) bdRepairer

var _ bdRepairer = (*beads.Beads)(nil)

// repair returns the bd repair client for dir: bd pinned to dir's own
// database (no prefix routing), unless the context carries an opener.
func (ctx *CheckContext) repair(dir string) bdRepairer {
	if ctx != nil && ctx.openRepair != nil {
		return ctx.openRepair(dir)
	}
	return ctx.beadsRigLocal(dir)
}
