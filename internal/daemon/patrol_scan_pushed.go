package daemon

import (
	"errors"
	"fmt"
)

// PushedSinceSling implements patrolscan.Env (gt-hgb91h): origin's head for the
// seat's work branch when that head was committed after the bead's sling clock
// began, "" when the branch is not on origin or its head predates the sling, and
// an error when the read cannot answer.
//
// The branch comes from the handle the overrun guard's stop works through, so a
// sandbox seat is read in its clone and a host seat in its worktree. The clock
// is the guard's own (overrunSlungReader.slungAt) and the decision its
// originHeadSince.
func (h *patrolScanHost) PushedSinceSling(rig, name string) (string, error) {
	bead, err := h.seatWorkBead(rig, name)
	if err != nil {
		return "", err
	}
	st, err := h.d.workBeadState(bead)
	if err != nil {
		return "", fmt.Errorf("read the work bead %s of %s/%s: %w", bead, rig, name, err)
	}
	seat := overrunSeat{Rig: rig, Polecat: name, Bead: bead, SubmittedAt: st.submittedAt, EscalatedEnd: st.escalatedAt}
	g, err := h.d.overrunSeatGit(seat)
	if err != nil {
		return "", fmt.Errorf("open the worktree of %s/%s: %w", rig, name, err)
	}
	branch, err := seatBranch(g)
	if err != nil {
		return "", fmt.Errorf("%s/%s: %w", rig, name, err)
	}
	slung, ok := (&overrunSlungReader{d: h.d}).slungAt(seat)
	if !ok || slung.IsZero() {
		return "", fmt.Errorf("the sling clock of %s for %s/%s cannot be read", bead, rig, name)
	}
	// originHeadSince answers "" for an origin it cannot reach, which the stop
	// reads as "not finished work". The Env contract needs that failed read as
	// an error, so origin is asked here first; a branch origin does not hold
	// falls through to the "" originHeadSince gives it.
	if _, _, err := originBranchRead(g, branch); err != nil {
		return "", fmt.Errorf("%s/%s: %w", rig, name, err)
	}
	return originHeadSince(g, branch, slung), nil
}

// seatWorkBead is the bead whose sling clock dates the seat's run: the one its
// intent record names, or the work assigned to it when the record names none,
// the two reads the overrun guard's seat makes (overrunSeatFor). A seat that
// names no bead has no clock to date a head from, so that is an error.
func (h *patrolScanHost) seatWorkBead(rig, name string) (string, error) {
	rec, err := h.Intent(rig, name)
	if err != nil {
		return "", fmt.Errorf("read the intent record of %s/%s: %w", rig, name, err)
	}
	if rec.WorkBead != "" {
		return rec.WorkBead, nil
	}
	bead, err := h.d.assignedActiveWorkBead(rig, rig+"/polecats/"+name)
	if err != nil {
		return "", fmt.Errorf("read the work assigned to %s/%s: %w", rig, name, err)
	}
	if bead == "" {
		return "", fmt.Errorf("%s/%s holds no work bead", rig, name)
	}
	return bead, nil
}

// seatBranch is the branch the seat's work is on, read from the handle the
// overrun guard's stop would push from. A checkout on no branch cannot name
// one, so it is an error: the caller would otherwise read a failed read as no
// push.
func seatBranch(g daemonGit) (string, error) {
	branch, err := g.CurrentBranch()
	if err != nil {
		return "", fmt.Errorf("read the seat's branch: %w", err)
	}
	if branch == "" || branch == "HEAD" {
		return "", errors.New("the seat's worktree is on no branch")
	}
	return branch, nil
}
