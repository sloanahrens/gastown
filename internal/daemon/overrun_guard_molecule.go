package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// overrunMoleculeReason is the close reason recorded on the steps and root of a
// molecule the guard ends.
const overrunMoleculeReason = "overrun stop: bead handed back"

// overrunHandBackStore is what a hand-back that also ends the bead's molecule
// reads and writes. *beads.Beads satisfies it; a test passes beadsfake.
type overrunHandBackStore interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	AddCommentAs(id, author, comment string) error
	Children(parentID string) ([]*beads.Issue, error)
	ForceCloseWithReason(reason string, ids ...string) error
}

// handBackEndingMolecule hands the stopped seat's bead back (polecat.HandBackNoted,
// or polecat.HandBackRework when seat.ReworkSHA names finished work already on
// origin) and then ends the molecule the bead carried, so a stopped run
// leaves no open molecule beside the one a re-dispatch pours. The bead is
// closed by neither step, which is why the closeMoleculeOfClosedBead path
// (gt-x1d8s) never sees it.
//
// The molecule is named from the bead before the hand-back: once the bead is
// open and unassigned the dispatcher can re-sling it and replace the
// attachment, and the new run's molecule is not this one to end. It fails
// closed: a molecule that cannot be read or closed is logged and the hand-back
// stands, since the bead going back to the pool matters more than the
// molecule, and releasing the seat must not wait on it. The stop does not
// repeat a hand-back for the molecule alone; it repeats one only when it
// could not save its progress, and that repeat tries the molecule again. A
// hand-back that is skipped or fails ends nothing.
func handBackEndingMolecule(ctx context.Context, store overrunHandBackStore, logf func(format string, args ...any),
	seat overrunSeat, elapsed time.Duration, note string) error {
	molID := ""
	if is, err := store.Show(seat.Bead); err != nil {
		logf("overrun_guard: %s/%s: cannot read %s for its molecule; handing it back without ending one: %v",
			seat.Rig, seat.Polecat, seat.Bead, err)
	} else if att := beads.ParseAttachmentFields(is); att != nil {
		molID = strings.TrimSpace(att.AttachedMolecule)
	}

	w := polecat.NewBeadWriter(store, overrunActor)
	assignee := overrunSeatAssignee(seat)
	var err error
	if seat.ReworkSHA != "" {
		err = polecat.HandBackRework(ctx, w, seat.Bead, assignee, seat.Branch, seat.ReworkSHA, elapsed, note)
	} else {
		err = polecat.HandBackNoted(ctx, w, seat.Bead, assignee, seat.Branch, elapsed, note)
	}
	if err != nil {
		return err
	}
	if molID == "" {
		return nil
	}
	n, err := endOverrunMolecule(store, molID)
	if err != nil {
		logf("overrun_guard: %s/%s: bead %s handed back but its molecule %s was left open: %v",
			seat.Rig, seat.Polecat, seat.Bead, molID, err)
		return nil
	}
	if n > 0 {
		logf("overrun_guard: %s/%s: ended molecule %s of %s (%d bead(s) closed)", seat.Rig, seat.Polecat, molID, seat.Bead, n)
	}
	return nil
}

// endOverrunMolecule closes molID's open descendants and then molID, and
// reports how many beads it closed. The close is forced, as gt done's close of
// the root is: the stopped polecat's steps are still assigned to it, which bd
// refuses to close for any other actor, and the run is abandoned, so there is
// no unfinished work for bd's fences to protect. A molecule that is gone or
// already ended closes nothing.
func endOverrunMolecule(store overrunHandBackStore, molID string) (int, error) {
	root, err := store.Show(molID)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read molecule %s: %w", molID, err)
	case beads.IssueStatus(strings.TrimSpace(root.Status)).IsTerminal():
		return 0, nil
	}
	ids, err := openMoleculeDescendants(store, molID)
	if err != nil {
		return 0, err
	}
	ids = append(ids, molID)
	err = store.ForceCloseWithReason(overrunMoleculeReason, ids...)
	n := len(beads.ClosedIDs(ids, err))
	if err != nil {
		return n, fmt.Errorf("close molecule %s: %w", molID, err)
	}
	return n, nil
}

// openMoleculeDescendants lists parentID's descendants that are not closed,
// through Children (bd show --children), which sees the ephemeral step wisps
// that bd list --parent misses (gt-43t7).
func openMoleculeDescendants(store overrunHandBackStore, parentID string) ([]string, error) {
	children, err := store.Children(parentID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", parentID, err)
	}
	var out []string
	for _, child := range children {
		deeper, err := openMoleculeDescendants(store, child.ID)
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
