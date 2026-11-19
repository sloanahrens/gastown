package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// PushedSinceSling implements patrolscan.Env (gt-hgb91h): origin's head for the
// seat's work branch when that head was committed after the bead's sling clock
// began, "" when the branch is not on origin or its head predates the sling, and
// an error when the read cannot answer.
//
// The branch comes from the handle the overrun guard's stop works through, so a
// sandbox seat is read in its clone and a host seat in its worktree. The clock
// is the guard's own (overrunSlungReader.slungAt) and the decision is
// pushedHeadSince, originHeadSince's steps with each failed read returned.
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
	return pushedHeadSince(g, branch, slung)
}

// pushedHeadSince is originHeadSince's decision with every failed read returned
// as an error. originHeadSince answers "" for each failure, which the overrun
// stop reads as "not finished work"; the Env contract reads "" as "not pushed",
// so the same steps here keep the failure. A read that answers "not on origin",
// "origin's head does not descend from the local tip" or "origin's head predates
// the sling" returns "" with no error. The caller sets slingAt.
func pushedHeadSince(g daemonGit, branch string, slingAt time.Time) (string, error) {
	tip, err := g.Rev("refs/heads/" + branch)
	if err != nil {
		return "", fmt.Errorf("read the local head of %s: %w", branch, err)
	}
	if tip == "" {
		return "", fmt.Errorf("the local head of %s is empty", branch)
	}
	origin, held, err := originBranchRead(g, branch)
	if err != nil {
		return "", err
	}
	if !held {
		return "", nil
	}
	if origin != tip {
		fetched, err := g.HasObject(origin)
		if err != nil {
			return "", fmt.Errorf("check origin's head %s of %s is fetched: %w", origin, branch, err)
		}
		if !fetched {
			if err := g.FetchRefspecWithTimeout("origin", "refs/heads/"+branch, git.RemoteQueryTimeout); err != nil {
				return "", fmt.Errorf("fetch origin's %s: %w", branch, err)
			}
		}
		ancestor, err := g.IsAncestor(tip, origin)
		if err != nil {
			return "", fmt.Errorf("check origin's head %s of %s descends from the local tip: %w", origin, branch, err)
		}
		if !ancestor {
			return "", nil
		}
	}
	at, err := g.CommitTime(origin)
	if err != nil {
		return "", fmt.Errorf("read the commit time of origin's head %s: %w", origin, err)
	}
	if !at.After(slingAt) {
		return "", nil
	}
	return origin, nil
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
