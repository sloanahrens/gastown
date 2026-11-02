package cmd

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The spec dispatcher's target-claim check (gt-n6jbw). The peer set is the
// duplicate guard's pool, whose fetch is cached, so the rule costs the slings
// that follow no read of their own.

// specCandidateClaim reduces the bead about to be slung to its claim: the test
// files and test identifiers its own text names. The paths go through
// specHoldFiles so the candidate and its peers are read by one rule — a
// document a bead only cites is not a target.
func specCandidateClaim(spec specdispatch.Spec) specdispatch.Claimant {
	refs := extractOverlapRefs(spec.Title, spec.Description, spec.Design, spec.Notes)
	return specdispatch.Claimant{
		ID:        spec.ID,
		Status:    spec.Status,
		Assignee:  spec.Assignee,
		Priority:  spec.Priority,
		CreatedAt: spec.CreatedAt,
		Paths:     specHoldFiles(refs.Files, spec.Description, refs.Text),
		Tests:     refs.Tests,
	}
}

// claimOf reduces one pool row to the claim the target rule weighs. The pool
// row carries the same fields specCandidateClaim reads off the candidate, so
// the two sides of the comparison cannot disagree about what a bead names.
func claimOf(c duplicateCandidate) specdispatch.Claimant {
	return specdispatch.Claimant{
		ID:       c.ID,
		Status:   c.Status,
		Assignee: c.Ready.Assignee,
		// Labels come from the list row, so a handoff note is recognized as
		// one even when its design and notes went unread (gt-d5f5q).
		Labels:    c.Ready.Labels,
		Priority:  c.Priority,
		CreatedAt: c.CreatedAt,
		ClosedAt:  c.ClosedAt,
		Landed:    c.Landed,
		// An unread claim names no full path TargetClaimHold can compare, so it
		// holds nothing (gt-dnp9c); UnreadClaimPeers reads the flag so the
		// tick can report the peer rather than treat it as idle.
		ClaimUnread: c.ClaimsUnread,
		Paths:       specHoldFiles(c.Refs.Files, c.Ready.Description, c.Refs.Text),
		Tests:       c.Refs.Tests,
	}
}

// specTargetClaims reads the claims a candidate's rig holds, for the tick's
// target-claim rule. A rig whose beads directory cannot be resolved, or whose
// pool will not load, is an error: the tick holds the candidate rather than
// dispatching on a claim set it could not read.
func specTargetClaims(townRoot, beadID string) ([]specdispatch.Claimant, error) {
	return specTargetClaimsFrom(beadID, duplicateBeadsDir(townRoot, beadID), slingDuplicatePools.poolFor)
}

// specTargetClaimsFrom is specTargetClaims with the beads directory resolution
// and the pool read injected, so a failed read is driven by a test rather than
// by the host's bead store.
func specTargetClaimsFrom(beadID, beadsDir string, poolFor func(string) ([]duplicateCandidate, error)) ([]specdispatch.Claimant, error) {
	if beadsDir == "" {
		return nil, fmt.Errorf("cannot resolve the beads directory of %s", beadID)
	}
	pool, err := poolFor(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("cannot read the beads of %s: %w", beadsDir, err)
	}
	out := make([]specdispatch.Claimant, 0, len(pool))
	for _, row := range pool {
		// An epic enumerates its children's tests rather than naming one
		// target, so it claims no line of its own (sling.Duplicate.IsEpic).
		if row.IsEpic() {
			continue
		}
		out = append(out, claimOf(row))
	}
	return out, nil
}

// claims is the env's target claims for a bead's rig. A nil seam is a caller
// that never wired the question, and an unwired claim reader holds the
// candidate rather than reading as "nothing claims it".
func (e specDispatchEnv) claims(beadID string) ([]specdispatch.Claimant, error) {
	if e.Claims == nil {
		return nil, errors.New("no claim reader is configured")
	}
	return e.Claims(beadID)
}
