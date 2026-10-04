package land

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// moleculeBeads is the slice of Beads closing an attached molecule needs.
type moleculeBeads interface {
	Show(id string) (*beads.Issue, error)
	Children(parentID string) ([]*beads.Issue, error)
	CloseWithReason(reason string, ids ...string) error
}

// closeAttachedMolecule closes the molecule a closed work bead carries in its
// attached_molecule field: its step wisps first, then the root, unforced.
// A bead that is not closed keeps its molecule — the molecule is the
// polecat's workflow, and only the landing ends it (gt-oqz0r).
//
// Best effort. A molecule bd refuses to close is left open with a line in the
// landing log: the landing is already on the target, and failing the record
// would have every repair pass retry a close that cannot succeed.
func (l *Lander) closeAttachedMolecule(beadID string) {
	issue, err := l.Beads.Show(beadID)
	if err != nil {
		l.logf("%s: reading the bead for its molecule: %v", beadID, err)
		return
	}
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
		return
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil {
		return
	}
	molID := strings.TrimSpace(attachment.AttachedMolecule)
	if molID == "" {
		return
	}
	closed, err := closeMolecule(l.Beads, molID)
	if err != nil {
		l.logf("%s: molecule %s left open after landing: %v", beadID, molID, err)
		return
	}
	if closed > 0 {
		l.logf("%s: closed %d bead(s) of molecule %s", beadID, closed, molID)
	}
}

// closeMolecule closes rootID's open descendants and then rootID, unforced,
// and reports how many beads it closed. Root last, because bd refuses a
// parent with open children; the work bead the molecule is bonded to is
// closed before this runs, so the root's own bond is no longer a fence.
func closeMolecule(b moleculeBeads, rootID string) (int, error) {
	steps, err := openDescendants(b, rootID)
	if err != nil {
		return 0, err
	}
	closed, err := closeUnforced(b, steps)
	if err != nil {
		return closed, err
	}
	root, err := b.Show(rootID)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return closed, nil
	case err != nil:
		return closed, fmt.Errorf("reading molecule %s: %w", rootID, err)
	case beads.IssueStatus(strings.TrimSpace(root.Status)).IsTerminal():
		return closed, nil
	}
	n, err := closeUnforced(b, []string{rootID})
	return closed + n, err
}

// openDescendants lists parentID's descendants that are not closed, deepest
// first, which is the order bd lets them close in.
func openDescendants(b moleculeBeads, parentID string) ([]string, error) {
	children, err := b.Children(parentID)
	if err != nil {
		return nil, fmt.Errorf("listing children of %s: %w", parentID, err)
	}
	var out []string
	for _, child := range children {
		deeper, err := openDescendants(b, child.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, deeper...)
		if !beads.IssueStatus(strings.TrimSpace(child.Status)).IsTerminal() {
			out = append(out, child.ID)
		}
	}
	return out, nil
}

// closeUnforced closes ids with an unforced bd close, retrying the ones bd
// refuses while a pass still closes something, and reports how many closed.
// bd refuses an issue whose blocker is open at its turn even when the blocker
// is later in the same batch, and a molecule's steps are a chain of blocks
// dependencies, so one pass closes only part of a chain (gt-oqz0r).
func closeUnforced(b moleculeBeads, ids []string) (int, error) {
	closed := 0
	pending := ids
	for len(pending) > 0 {
		err := b.CloseWithReason(closeReasonLanded, pending...)
		if err == nil {
			return closed + len(pending), nil
		}
		var pe *beads.PartialCloseError
		if !errors.As(err, &pe) || len(pe.Closed) == 0 {
			return closed, fmt.Errorf("closing %d of %d bead(s): %w", len(pending), len(ids), err)
		}
		closed += len(pe.Closed)
		pending = pe.NotClosed
	}
	return closed, nil
}

// closeReasonLanded is the close reason the landing records on a molecule it
// closes.
const closeReasonLanded = "landed"
