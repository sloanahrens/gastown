package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// PruneRecheck returns the re-check ApplyPrune calls for one delete candidate.
// It reads the ledger and the green refs again and refuses, naming the tag, one
// they now keep or cannot decide. keepNewest, the plan's count, is the tag's
// rank too: with no listing read, only the pairs and the green refs protect.
func PruneRecheck(ledger PruneLedger, green func() (map[string]bool, error), keepNewest int) func(PruneEntry) error {
	return func(e PruneEntry) error {
		if green == nil {
			return pruneRecheckRefusal(e, errors.New("no green refs reader is wired"))
		}
		installed, previous, err := ReadPrunePairs(ledger)
		if err != nil {
			return pruneRecheckRefusal(e, err)
		}
		greenRefs, err := green()
		if err != nil {
			return pruneRecheckRefusal(e, fmt.Errorf("reading the green refs: %w", err))
		}
		reason := keepReason(e.Image, PruneTag{Name: e.Tag, Digest: e.Digest}, keepNewest, keepNewest,
			prunePinnedDigests(installed, previous), greenRefs, installed.Bd == "")
		if reason != "" {
			return pruneRecheckRefusal(e, fmt.Errorf("the keep rule protects it now (%s)", reason))
		}
		return nil
	}
}

// pruneRecheckRefusal names the candidate a re-check refused, so the skip and
// the run's failure both say which tag was protected.
func pruneRecheckRefusal(e PruneEntry, err error) error {
	return fmt.Errorf("the re-check of %s/%s: %w", e.Image, e.Tag, err)
}

// prunePinnedDigests maps each digest a pair names to the reason that keeps it,
// the installed pair winning a digest both pairs name. A ledger pair is a
// pullable reference, repo@sha256:<hex>, while a tag's digest is sha256:<hex>,
// so the digest after the last '@' is what pins a tag (gt-ikt4il).
func prunePinnedDigests(installed, previous ImagePair) map[string]string {
	pinned := map[string]string{}
	for _, p := range []struct {
		pair   ImagePair
		reason string
	}{{installed, "installed pair"}, {previous, "previous pair"}} {
		for _, ref := range []string{p.pair.Daemon, p.pair.Polecat, p.pair.Bd} {
			d := ref
			if i := strings.LastIndexByte(ref, '@'); i >= 0 {
				d = ref[i+1:]
			}
			if _, ok := pinned[d]; d != "" && !ok {
				pinned[d] = p.reason
			}
		}
	}
	return pinned
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
