package landworker

import (
	"context"
	"fmt"
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
// landing preserves because a rework is pushed to a new branch. The head that
// landed stays reachable at refs/landed/<bead>, written by Land before this
// reap can run (gt-as9v8), so the delete takes no history with it. The rig
// repository's copy of the branch that landed goes with it, and only that one
// (deleteLocalBranch, gt-kne58, gt-xpmr7): reaping origin alone left every
// branch the rig ever pushed in its refs/heads forever, and taking the rig's
// copy of an earlier attempt would drop whatever the seat made past the head
// that landed. Best effort: a failed list or delete is logged, and the
// landing's record is already written, so nothing here fails or re-delays it.
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
			w.logf("%s: deleting %s on origin: %v", beadID, branch, err)
		} else {
			deleted++
		}
		// The local half runs whether or not origin's delete did: a branch
		// already gone from origin is exactly one the rig still holds.
		w.deleteLocalBranch(beadID, branch, ref.Hash, false, w.logf)
	}
	if deleted > 0 {
		w.logf("%s: deleted %d stale polecat branch(es) on origin", beadID, deleted)
	}
	return found
}

// deleteLocalBranch deletes the rig repository's copy of a polecat branch
// whose origin copy has just been reaped (gt-kne58), keeping the branch
// unless it is still a copy of the commit that landed. The landing reap and
// the leftover sweep both come through here, so the rig repository loses a
// branch only by one rule; logf is the caller's, because the sweep reports a
// branch at most once a run and the reap reports every bead once.
//
// A branch goes only while its tip is the landed head — the commit whose
// content the landing took, so the delete drops nothing the target does not
// already hold — and the delete is pinned at that head: a seat that committed
// past the head it landed carries work no landing took, and the rig
// repository's copy is then the only one left, while a branch that moves
// between the check and the delete fails the pin and is kept (gt-xpmr7).
// Git's own merged test can stand in for neither:
// git branch -d reads the repository's own HEAD branch, which here is the rig
// repository's main, and a landing moves the target on the remote, not here
// (gt-22ewa). Everything else keeps the branch, every lookup failure
// included: an unmerged branch deleted by mistake is the one outcome nothing
// here can undo.
func (w *Worker) deleteLocalBranch(beadID, branch, hash string, provenByCaller bool, logf func(format string, args ...any)) {
	head, unproven := w.landedHead(beadID, hash, provenByCaller)
	if unproven != "" {
		logf("%s: kept local %s: %s", beadID, branch, unproven)
		return
	}
	tip, err := w.Remote.LocalBranchTip(branch)
	if err != nil {
		logf("%s: kept local %s: reading it: %v", beadID, branch, err)
		return
	}
	if tip == "" {
		// Already gone: a branch reaped twice has nothing to take again, and
		// that is not an error (gt-kne58).
		return
	}
	if tip != head {
		logf("%s: kept local %s: its tip %s is not the landed head %s", beadID, branch, short(tip), short(head))
		return
	}
	deleted, err := w.Remote.DeleteLocalBranch(branch, head)
	if err != nil {
		logf("%s: keeping the local %s: %v", beadID, branch, err)
		return
	}
	if deleted {
		logf("%s: deleted the local polecat branch %s", beadID, branch)
	}
}

// landedHead is the commit a polecat branch of beadID is a copy of: the head
// that landed there, which alone earns the branch its delete. head is "" and
// unproven says what stopped the search when nothing proves one.
//
// provenByCaller is a proof the caller already holds — the sweep passes the
// hash it reached here on, having read the target's content — and a caller
// with none passes false to have one looked for: the head the landings file's
// latest record for the bead names, or, where there is no record, the hash
// itself, where the branch the worker lands contains it. The record's head
// and not the record (gt-xpmr7): the bead having landed somewhere says
// nothing about the branch in hand, which may be an earlier attempt the
// landing never took.
func (w *Worker) landedHead(beadID, hash string, provenByCaller bool) (head, unproven string) {
	if provenByCaller {
		return hash, ""
	}
	if w.Landings != nil {
		rec, found, err := w.Landings.LatestForBead(beadID)
		if err != nil {
			return "", fmt.Sprintf("reading the landings file: %v", err)
		}
		if found {
			if rec.Head == "" {
				return "", "its landing record names no head"
			}
			return rec.Head, ""
		}
	}
	if w.WatchTarget == "" {
		return "", "no landing record and no branch to have merged it into"
	}
	merged, err := w.Remote.Contains(w.WatchTarget, hash)
	if err != nil {
		return "", fmt.Sprintf("testing it against %s: %v", w.WatchTarget, err)
	}
	if !merged {
		return "", "not merged"
	}
	return hash, ""
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

// sweepLeftoverBranches deletes a bead's polecat branch on origin and in the
// rig repository for work that is finished and landed, whatever left them
// there: reapBeadBranches runs only in the moment right after a landing, so a
// restart in that window, a bead closed without a landing, and branches
// pushed as transport before the Forgejo cut-over keep their branch forever
// (gt-xz4ir, gt-4j1pl).
//
// Deleting is the riskier half, so a branch goes only when every rule holds:
// its bead is closed or has a landing record, its work is provably on the
// branch the worker lands (WatchTarget), and its tip is older than
// leftoverBranchMinAge. Anything else stays; a branch that is not provably
// merged is reported once per run for a human. The rig repository's own copy
// follows origin's through the reap's local half, so both halves go by one
// rule. Best effort throughout, like reapBeadBranches: a failed list, fetch,
// check or delete is logged and the pass goes on.
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
		// The rig repository's own copy goes with origin's, through the
		// landing reap's local half so one rule decides what the rig loses:
		// a branch the sweep got this far on is proven done and on the
		// target, and that proof is the one it passes down (gt-4j1pl).
		w.deleteLocalBranch(bead, branch, ref.Hash, true, func(format string, args ...any) {
			w.sweepLog(branch, format, args...)
		})
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
