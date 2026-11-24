package update

import (
	"context"
	"errors"
	"fmt"
)

// ErrPruneApplyRefused is returned by ApplyPrune before any call, when its
// arguments cannot run: a negative max, or a nil recheck or del.
var ErrPruneApplyRefused = errors.New("the prune apply refuses this request")

// PruneSkip is a delete candidate ApplyPrune did not delete because its
// re-check failed. Err is the re-check's error.
type PruneSkip struct {
	Entry PruneEntry
	Err   error
}

// PruneApplyResult is what ApplyPrune did. Deleted and Skipped are in plan
// order. Failed is true when an entry was skipped or a delete failed, so the
// command exits non-zero.
type PruneApplyResult struct {
	Deleted []PruneEntry
	Skipped []PruneSkip
	Failed  bool
}

// ApplyPrune deletes the plan's delete candidates in order, making at most
// maxDeletes del calls. For each candidate it calls recheck, then del. A
// recheck error skips the candidate without spending a delete and the run goes
// on. A del error stops the run at once, with no retry. Keep entries never
// reach recheck or del. Nothing here touches the network.
func ApplyPrune(ctx context.Context, plan PrunePlan, maxDeletes int, recheck func(PruneEntry) error, del func(context.Context, PruneEntry) error) (PruneApplyResult, error) {
	switch {
	case maxDeletes < 0:
		return PruneApplyResult{}, fmt.Errorf("%w: the delete limit is %d", ErrPruneApplyRefused, maxDeletes)
	case recheck == nil:
		return PruneApplyResult{}, fmt.Errorf("%w: no re-check function", ErrPruneApplyRefused)
	case del == nil:
		return PruneApplyResult{}, fmt.Errorf("%w: no delete function", ErrPruneApplyRefused)
	}

	var res PruneApplyResult
	for _, e := range plan.Delete {
		if len(res.Deleted) == maxDeletes {
			break
		}
		if err := recheck(e); err != nil {
			res.Skipped = append(res.Skipped, PruneSkip{Entry: e, Err: err})
			res.Failed = true
			continue
		}
		if err := del(ctx, e); err != nil {
			res.Failed = true
			return res, fmt.Errorf("delete %s/%s: %w", e.Image, e.Tag, err)
		}
		res.Deleted = append(res.Deleted, e)
	}
	return res, nil
}
