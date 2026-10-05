package landworker

import (
	"context"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// polecatRefPrefix is the origin ref space of a polecat's work branches; a
// branch is polecat/<polecat>/<bead>+<mutation>.
const polecatRefPrefix = "refs/heads/polecat/"

// RemoteRef is an origin ref and the commit it points at.
type RemoteRef struct {
	Name string
	Hash string
}

// reapBeadBranches deletes origin's branches for a bead that has just landed:
// the branch it landed and every earlier attempt for the bead, which no later
// landing preserves because a rework is pushed to a new branch. Best effort:
// a failed list or delete is logged, and the landing's record is already
// written, so nothing here fails or re-delays it.
//
// It returns every branch it found for the bead, deleted or not, so the
// landing path can also refresh the seat of each polecat that authored one
// (gt-fn9e6.55).
func (w *Worker) reapBeadBranches(beadID string) []string {
	if beadID == "" {
		return nil
	}
	refs, err := w.Remote.ListRemoteRefs(polecatRefPrefix)
	if err != nil {
		w.logf("%s: listing the polecat branches to delete: %v", beadID, err)
		return nil
	}
	var found []string
	deleted := 0
	for _, ref := range refs {
		if !branchForBead(ref.Name, beadID) {
			continue
		}
		branch := strings.TrimPrefix(ref.Name, "refs/heads/")
		found = append(found, branch)
		if err := w.Remote.DeleteRemoteBranchIfAt(branch, ref.Hash); err != nil {
			w.logf("%s: deleting %s: %v", beadID, branch, err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		w.logf("%s: deleted %d stale polecat branch(es) on origin", beadID, deleted)
	}
	return found
}

// polecatBranchPrefix is polecatRefPrefix as a branch is named: without the
// refs/heads/ its ref hangs under.
const polecatBranchPrefix = "polecat/"

// authorPolecat returns the polecat a polecat branch names, or "" when branch
// is not one. A branch is polecat/<polecat>/<bead>+<mutation>.
func authorPolecat(branch string) string {
	rest, ok := strings.CutPrefix(branch, polecatBranchPrefix)
	if !ok {
		return ""
	}
	polecat, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return polecat
}

// branchForBead reports whether a full origin ref name is a polecat branch for
// beadID: polecat/<polecat>/<beadID>+<mutation>. The id is matched through its
// "+" so a child such as gt-x.10 is never taken for gt-x.1.
func branchForBead(refName, beadID string) bool {
	rest, ok := strings.CutPrefix(refName, polecatRefPrefix)
	if !ok {
		return false
	}
	worker, bead, ok := strings.Cut(rest, "/")
	if !ok || worker == "" {
		return false
	}
	return strings.HasPrefix(bead, beadID+"+")
}

// beadForBranch is the bead a polecat branch names, "" when branch is not one.
// The id from the branch reaches bd as an argument, and a branch name is
// whatever its pusher made it, so an id starting with "-" (which bd would read
// as a flag) is no id at all.
func beadForBranch(branch string) string {
	rest, ok := strings.CutPrefix(branch, polecatBranchPrefix)
	if !ok {
		return ""
	}
	polecat, rest, ok := strings.Cut(rest, "/")
	if !ok || polecat == "" {
		return ""
	}
	bead, mutation, ok := strings.Cut(rest, "+")
	if !ok || bead == "" || mutation == "" || !startsBeadID(bead[0]) {
		return ""
	}
	return bead
}

// startsBeadID reports whether c can open a bead id: an alphanumeric, never
// the "-" of a flag.
func startsBeadID(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// DefaultSweepInterval is how often a worker sweeps origin for leftover
// polecat branches when Worker.SweepInterval is unset.
const DefaultSweepInterval = time.Hour

// leftoverBranchMinAge is how old a branch tip must be before the sweep may
// delete it: a branch pushed minutes ago may belong to a landing or a rework
// still in flight.
const leftoverBranchMinAge = time.Hour

// sweepIfDue runs the leftover-branch sweep when SweepInterval has elapsed
// since the last one. The first pass of a worker run sweeps whatever the
// interval, because the run starts after a restart and a restart is what
// leaves a just-landed bead's branches behind.
func (w *Worker) sweepIfDue(ctx context.Context) {
	if w.WatchTarget == "" || ctx.Err() != nil {
		return
	}
	interval := w.SweepInterval
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	now := w.now()
	if !w.sweptAt.IsZero() && now.Sub(w.sweptAt) < interval {
		return
	}
	w.sweptAt = now
	w.sweepLeftoverBranches(ctx)
}

// sweepLeftoverBranches deletes origin's polecat branches for work that is
// finished and landed, whatever left them there: reapBeadBranches runs only
// in the moment right after a landing, so a restart in that window, a bead
// closed without a landing, and branches pushed as transport before the
// Forgejo cut-over keep their branch on origin forever (gt-xz4ir).
//
// Deleting is the riskier half, so a branch goes only when every rule holds:
// its bead is closed or has a landing record, its work is provably on the
// branch the worker lands (WatchTarget), and its tip is older than
// leftoverBranchMinAge. Anything else stays; a branch that is not provably
// merged is reported once per run for a human. Best effort throughout, like
// reapBeadBranches: a failed list, fetch, check or delete is logged and the
// pass goes on.
func (w *Worker) sweepLeftoverBranches(ctx context.Context) {
	refs, err := w.Remote.ListRemoteRefs(polecatRefPrefix)
	if err != nil {
		w.logf("sweeping the leftover polecat branches: listing: %v", err)
		return
	}
	deleted := 0
	for _, ref := range refs {
		if ctx.Err() != nil {
			return
		}
		branch := strings.TrimPrefix(ref.Name, "refs/heads/")
		bead := beadForBranch(branch)
		if bead == "" {
			continue
		}
		done, err := w.sweepBeadDone(bead)
		if err != nil {
			w.sweepLog(branch, "leftover polecat branch %s: reading bead %s: %v", branch, bead, err)
			continue
		}
		if !done {
			continue
		}
		merged, err := w.Remote.BranchLandedOn(w.WatchTarget, branch, ref.Hash)
		if err != nil {
			w.sweepLog(branch, "leftover polecat branch %s: %v", branch, err)
			continue
		}
		if !merged {
			w.sweepLog(branch, "kept %s: not merged into %s", branch, w.WatchTarget)
			continue
		}
		at, err := w.Remote.CommitTime(ref.Hash)
		if err != nil {
			w.sweepLog(branch, "leftover polecat branch %s: reading its tip's time: %v", branch, err)
			continue
		}
		if w.now().Sub(at) < leftoverBranchMinAge {
			continue
		}
		if err := w.Remote.DeleteRemoteBranchIfAt(branch, ref.Hash); err != nil {
			w.sweepLog(branch, "leftover polecat branch %s: deleting: %v", branch, err)
			continue
		}
		w.logf("deleted the leftover polecat branch %s (bead %s)", branch, bead)
		deleted++
	}
	if deleted > 0 {
		w.logf("swept %d leftover polecat branch(es) on origin", deleted)
	}
}

// sweepBeadDone reports whether a bead's origin branches are the sweep's
// business: the bead is closed, or the landings file records a landing for
// it. Every other state — open, in progress, blocked, deferred, hooked, or
// one bd cannot answer for — keeps its branches.
func (w *Worker) sweepBeadDone(beadID string) (bool, error) {
	if w.Landings != nil {
		if _, found, err := w.Landings.LatestForBead(beadID); err != nil {
			return false, err
		} else if found {
			return true, nil
		}
	}
	issue, err := w.Beads.Show(beadID)
	if err != nil {
		return false, err
	}
	if issue == nil {
		return false, nil
	}
	return beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal(), nil
}

// sweepLog logs one line about a leftover branch, at most once per branch per
// worker run: the sweep repeats every SweepInterval, and a branch that is
// staying is not news every time.
func (w *Worker) sweepLog(branch, format string, args ...any) {
	if w.sweepLogged == nil {
		w.sweepLogged = map[string]bool{}
	}
	if w.sweepLogged[branch] {
		return
	}
	w.sweepLogged[branch] = true
	w.logf(format, args...)
}
