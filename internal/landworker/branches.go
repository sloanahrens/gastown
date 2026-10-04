package landworker

import (
	"strings"
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
func (w *Worker) reapBeadBranches(beadID string) {
	if beadID == "" {
		return
	}
	refs, err := w.Remote.ListRemoteRefs(polecatRefPrefix)
	if err != nil {
		w.logf("%s: listing the polecat branches to delete: %v", beadID, err)
		return
	}
	deleted := 0
	for _, ref := range refs {
		if !branchForBead(ref.Name, beadID) {
			continue
		}
		branch := strings.TrimPrefix(ref.Name, "refs/heads/")
		if err := w.Remote.DeleteRemoteBranchIfAt(branch, ref.Hash); err != nil {
			w.logf("%s: deleting %s: %v", beadID, branch, err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		w.logf("%s: deleted %d stale polecat branch(es) on origin", beadID, deleted)
	}
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
