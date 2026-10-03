package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
)

// Dead-holder recovery: the git and bd half of returning a gone polecat's
// work bead to the ready queue (gt-gzhin.2). The decision — which bead, in
// what order, and everything that makes it fail closed — is patrolscan's;
// this file only answers "which branch?" and performs the two guarded writes.
//
// The branch lookup is the convoy feeder's origin listing, ported rather than
// called: the feeder is being deleted (gt-gzhin.6) and this tick becomes the
// one dead-holder path for every bead, convoy or not. Unlike the feeder it
// keeps no per-rig cache — one tick scans one rig once, and a rig whose
// holders are all alive asks nothing.

// deadHolderBranch returns the most recently generated polecat branch for
// beadID present on the rig's origin remote, "" when none is, and an error
// when origin could not be queried.
//
// Existence on origin, not unmerged work, is the question: the branch is
// recorded for the dispatcher to resume, and a branch whose work all landed
// leaves the bead open, which the resumed polecat closes. An unreadable
// remote is an error, never "no branch" — releasing on a failed query sends
// the work to a fresh polecat starting from main.
func (h *patrolScanHost) deadHolderBranch(rig, beadID string) (string, error) {
	list := h.listOriginBranches
	if list == nil {
		list = polecat.ListOriginPolecatBranches
	}
	branches, err := list(filepath.Join(h.town(), rig))
	if err != nil {
		return "", err
	}
	matches := polecat.MatchSurvivingBranches(branches, beadID)
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

// SurvivingBranch implements patrolscan.Env over the rig's own repo.
func (h *patrolScanHost) SurvivingBranch(rig, beadID string) (string, error) {
	return h.deadHolderBranch(rig, beadID)
}

// RecordResumeBranch implements patrolscan.Env: bd appends the line to the
// bead's notes, which is where the spec dispatcher reads it back (gt-gzhin.3).
func (h *patrolScanHost) RecordResumeBranch(rig, beadID, branch string) error {
	b := h.recoveryBeads()
	if err := b.AppendNotes(beadID, patrolscan.ResumeBranchNote(branch)); err != nil {
		return fmt.Errorf("bd update %s --append-notes: %w", beadID, err)
	}
	return nil
}

// Reopen implements patrolscan.Env: bd's --if-assignee guard releases the bead
// only while the dead holder still owns it, so the write is the "once" and a
// bead re-slung to a live polecat in the meantime is left to that polecat
// (gt-vm5g4).
func (h *patrolScanHost) Reopen(rig, beadID, assignee string) (bool, error) {
	reopened, err := h.recoveryBeads().ReleaseIfAssignee(beadID, assignee)
	if err != nil {
		return false, fmt.Errorf("bd update %s --if-assignee: %w", beadID, err)
	}
	return reopened, nil
}

// recoveryBeads is the mutating bd client for one recovery write, routed by
// the bead's own prefix from the town root the way Comment is.
func (h *patrolScanHost) recoveryBeads() beads.Client {
	env := bdMutationRoutingEnv(h.town())
	if h.openRecoveryBeads != nil {
		return h.openRecoveryBeads(env)
	}
	return beads.NewPlain(h.town(), env, beads.WithBin(h.d.bdPathOrDefault())).WithTimeout(patrolScanBdTimeout)
}
