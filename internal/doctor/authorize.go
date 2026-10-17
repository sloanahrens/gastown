package doctor

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// FixAuthLabel marks a bead as a genuine authorization record for a destructive
// doctor repair run by an agent (gt-638go.3, deep review G4-09). It mirrors the
// gt-61x guard on `gt dolt cleanup --force`: an agent may end a session or
// remove data only against a decision someone else recorded.
const FixAuthLabel = "doctor-fix-auth"

// ErrUnidentifiedFixActor refuses a destructive doctor repair whose actor is
// unknown: the process that would create the authorization bead is its only
// requester (gt-2kwxr, the gt-kz9k0 guard for gt doctor fix).
var ErrUnidentifiedFixActor = errors.New(`destructive doctor repair refused: the actor cannot be identified (gt-2kwxr)

GT_ROLE and BD_ACTOR are both unset and stdout is not an interactive terminal,
so any authorization bead would have to be created by this same unidentified
process.

Run as an agent with its identity set:
  GT_ROLE=gastown/polecats/<name> gt doctor fix <check> --authorized-by <bead-id>

Or run the repair from an interactive terminal as the operator`)

// ValidateFixAuthorization checks that the --authorized-by bead is a genuine
// authorization record, not any bead an agent happened to point at: the actor
// must be identified, and the bead must carry FixAuthLabel, must still be open,
// and must not have been created by the actor that wants to run the repair.
func ValidateFixAuthorization(issue *beads.Issue, actor string) error {
	if issue == nil {
		return fmt.Errorf("authorization bead not found")
	}
	// Refused outright, before the self-authorization check below (gt-2kwxr;
	// see doltserver.IsUnidentifiedActor).
	if doltserver.IsUnidentifiedActor(actor) {
		return ErrUnidentifiedFixActor
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
by the overseer:
  bd update %s --add-label %s  # or create a new bead carrying that label`,
			issue.ID, FixAuthLabel, issue.ID, FixAuthLabel)
	}
	if beads.IssueStatus(issue.Status).IsTerminal() {
		return fmt.Errorf("authorization bead %s is %s — a destructive doctor repair requires an open authorization (gt-638go.3)",
			issue.ID, issue.Status)
	}
	if issue.CreatedBy == actor {
		return fmt.Errorf("authorization bead %s was created by %s itself — self-authorization is not allowed (gt-638go.3); the bead must record an overseer decision",
			issue.ID, actor)
	}
	return nil
}
