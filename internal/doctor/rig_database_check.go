package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/townconfig"
)

// RigDatabaseCheck compares each registered rig's database name in the rig
// registry, which gastown reads, with the dolt_database of the rig's bd
// metadata.json, which bd reads (gt-y3pgh.11). A registry entry without a
// name is a warning, and --fix copies metadata.json's name into it
// (townconfig.AbsorbRigDatabases, the same step gt config migrate runs).
// Two different names are an error with no --fix: gastown and bd would
// address different databases, and only the operator knows which is right.
type RigDatabaseCheck struct {
	FixableCheck
}

// NewRigDatabaseCheck creates the rig-database check.
func NewRigDatabaseCheck() *RigDatabaseCheck {
	return &RigDatabaseCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "rig-database",
				CheckDescription: "Check that the rig registry and bd's metadata.json name the same database",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run reports rigs whose registry entry lacks the database name or names
// another one than metadata.json.
func (c *RigDatabaseCheck) Run(ctx *CheckContext) *CheckResult {
	states, err := townconfig.RigDatabaseStates(ctx.TownRoot)
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusError, Message: "Cannot read the rig registry", Details: []string{err.Error()}}
	}
	var drifted, missing []string
	for _, s := range states {
		switch {
		case s.Drifted():
			drifted = append(drifted, fmt.Sprintf("%s: registry dolt_database %q, %s %q", s.Rig, s.Registry, s.MetadataFile, s.Metadata))
		case s.Absorbable():
			missing = append(missing, fmt.Sprintf("%s: registry has no dolt_database; %s names %q", s.Rig, s.MetadataFile, s.Metadata))
		}
	}
	switch {
	case len(drifted) > 0:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d rig(s) name different databases in the registry and bd's metadata.json", len(drifted)),
			Details: append(drifted, missing...),
			FixHint: "Decide which database is the rig's, then make the registry entry's dolt_database and metadata.json agree by hand",
		}
	case len(missing) > 0:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d rig(s) have no database name in the registry", len(missing)),
			Details: missing,
			FixHint: "Run 'gt doctor --fix' (or 'gt config migrate') to record metadata.json's name in the registry",
		}
	}
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "The registry and bd's metadata.json name the same rig databases"}
}

// Fix records metadata.json's database name in every registry entry that
// has none. It never changes a recorded name.
func (c *RigDatabaseCheck) Fix(ctx *CheckContext) error {
	_, err := townconfig.AbsorbRigDatabases(ctx.TownRoot, false)
	return err
}
