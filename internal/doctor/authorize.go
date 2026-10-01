package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
)

// FixAuthLabel marks a bead as a genuine authorization record for a destructive
// doctor repair run by an agent (gt-638go.3, deep review G4-09). It mirrors the
// gt-61x guard on `gt dolt cleanup --force`: an agent may end a session or
// remove data only against a decision someone else recorded.
const FixAuthLabel = "doctor-fix-auth"

// ValidateFixAuthorization checks that the --authorized-by bead is a genuine
// authorization record, not any bead an agent happened to point at: it must
// carry FixAuthLabel, must still be open, and must not have been created by the
// actor that wants to run the repair.
func ValidateFixAuthorization(issue *beads.Issue, actor string) error {
	if issue == nil {
		return fmt.Errorf("authorization bead not found")
	}
	labeled := false
	for _, label := range issue.Labels {
		if label == FixAuthLabel {
			labeled = true
			break
		}
	}
	if !labeled {
		return fmt.Errorf(`bead %s is not an authorization record: missing the %q label (gt-638go.3)

A destructive doctor repair by an agent requires an authorization bead created
by the mayor/overseer:
  bd update %s --labels=%s   # or create a new bead carrying that label`,
			issue.ID, FixAuthLabel, issue.ID, FixAuthLabel)
	}
	if beads.IssueStatus(issue.Status).IsTerminal() {
		return fmt.Errorf("authorization bead %s is %s — a destructive doctor repair requires an open authorization (gt-638go.3)",
			issue.ID, issue.Status)
	}
	if actor != "" && issue.CreatedBy == actor {
		return fmt.Errorf("authorization bead %s was created by %s itself — self-authorization is not allowed (gt-638go.3); the bead must record a mayor/overseer decision",
			issue.ID, actor)
	}
	return nil
}
