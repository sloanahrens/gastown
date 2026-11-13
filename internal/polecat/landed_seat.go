package polecat

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
)

// SubmissionLanded reports whether the submission a seat's intent record holds
// is recorded as landed in its rig's landing log (gt-jhz3t0). It is the one
// definition the pool's occupancy, the patrol's seat cap and the polecat
// classifier all read, so they cannot disagree about a landed seat.
//
// A seat is landed only when a landing record for its work bead exists and
// was written at or after the submission the record holds. A landing that
// failed or was rejected writes no record, so it does not release the seat; a
// record older than the submission belongs to an earlier turn of the bead, so
// it does not release the resubmitted seat either. A submission with no
// SubmittedAt has nothing to outlive, so any record for its bead counts.
//
// A read failure is returned rather than read as "not landed" here: each
// caller decides what an unreadable landing log means for its count.
func SubmissionLanded(townRoot, rig string, rec intent.Record) (bool, error) {
	if !rec.Submitted() || rec.WorkBead == "" {
		return false, nil
	}
	landings, err := land.RigLandingsFile(townRoot, rig)
	if err != nil {
		return false, err
	}
	latest, found, err := landings.LatestForBead(rec.WorkBead)
	if err != nil {
		return false, fmt.Errorf("reading the landing record of %s: %w", rec.WorkBead, err)
	}
	if !found {
		return false, nil
	}
	if rec.SubmittedAt.IsZero() {
		return true, nil
	}
	return !latest.LandedAt.Before(rec.SubmittedAt), nil
}
