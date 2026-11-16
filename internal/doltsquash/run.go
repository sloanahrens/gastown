package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrRunMarkerHeld is what Marker.Hold returns while another run holds the
// marker. Run refuses such a run before it sends a statement.
var ErrRunMarkerHeld = errors.New("another run holds the run marker")

// Marker is the run marker: one run holds it for its whole length, so a second
// run that finds it held is refused instead of writing beside the first.
//
// Hold tries once and does not wait, so a run that finds the marker held is
// refused rather than queued. Run releases the marker on every way out, so a
// failed run does not keep the next check from running.
type Marker interface {
	// Hold takes the marker, or returns ErrRunMarkerHeld when another run holds
	// it.
	Hold() error
	// Release gives the marker back.
	Release() error
}

// RunInput is what one run reads: the Dolt seam, the run marker, the clock and
// sleeper the steps wait on, and the cut a due check chose.
type RunInput struct {
	Exec    Exec
	Marker  Marker
	Clock   Clock
	Sleeper Sleeper
	// Cut is the instant a commit must predate to be squashed, from Decide.
	Cut time.Time
}

// RunResult is what each step of one run did, one field per step in the order
// the steps run. "Design step N", here and in the package's other comments, is
// step N of "The run" in docs/plans/2026-11-15-dolt-squash-design.md: read that
// list before changing a step's order or its statements.
type RunResult struct {
	// Before is design step 3's record of main, read before the rebase.
	Before Before
	// Rebase is what design step 4 did.
	Rebase RebaseResult
	// Catchup is what design step 5 did.
	Catchup CatchupResult
	// Verified is design step 3's record read again at the last picked tip: the
	// state of main that design step 6 compares compact-work with. It is zero
	// when the run stopped before that record.
	Verified Before
	// Verify is what design step 6 found.
	Verify VerifyResult
	// Swap is what design steps 7 and 8 did.
	Swap SwapResult
}

// Run runs the squash over one database in the design's order: the BEFORE
// record, the rebase, the catch-up, verification, and the swap with its
// reconcile.
//
// The run holds the marker for its whole length and releases it on every way
// out, so a second run is refused before it sends a statement and a failed run
// does not keep the next check from running.
//
// Verification compares compact-work with main at the tip the catch-up stopped
// at, so main's state there is recorded after the catch-up: the BEFORE record
// describes the tip the rebase branched at, and the commits that landed since
// then are on compact-work. A record that reads main at another tip stops the
// run before verification, because the swap replays from the tip verification
// proved. A run that stops before the swap leaves main and the work branches as
// the stopped step left them.
//
// A step that fails, or a verification difference, stops the run and returns
// the result the steps reached. A swap that leaves a conflict is not an error:
// SwapResult.Conflict names the commits, and the caller raises the item.
func Run(ctx context.Context, in RunInput) (result RunResult, err error) {
	switch {
	case in.Exec == nil:
		return result, errors.New("doltsquash: the run has no Exec, so no step could read or write Dolt")
	case in.Marker == nil:
		return result, errors.New("doltsquash: the run has no marker, so a second run could start beside it")
	case in.Clock == nil:
		return result, errors.New("doltsquash: the run has no clock, so the catch-up cannot tell a quiet tip")
	case in.Sleeper == nil:
		return result, errors.New("doltsquash: the run has no sleeper, so the catch-up cannot wait out a quiet tip or a held working set")
	case in.Cut.IsZero():
		return result, errors.New("doltsquash: the run has no cut, so the rebase would mark no commit for squashing")
	}
	if err = ctx.Err(); err != nil {
		return result, fmt.Errorf("doltsquash: the run did not start: %w", err)
	}
	if err = in.Marker.Hold(); err != nil {
		return result, fmt.Errorf("doltsquash: the run marker: %w", err)
	}
	defer func() {
		if releaseErr := in.Marker.Release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("doltsquash: releasing the run marker: %w", releaseErr))
		}
	}()
	if result.Before, err = RecordBefore(ctx, in.Exec); err != nil {
		return result, err
	}
	if result.Rebase, err = RebaseHistory(ctx, in.Exec, result.Before, in.Cut); err != nil {
		return result, err
	}
	if result.Catchup, err = Catchup(ctx, in.Exec, result.Before.Tip, in.Clock, in.Sleeper); err != nil {
		return result, err
	}
	if result.Verified, err = RecordBefore(ctx, in.Exec); err != nil {
		return result, err
	}
	// Verification proves compact-work equal to main at the recorded tip, and the
	// swap replays the old main's tail from the tip it verified against. A record
	// that read a tip the catch-up never stopped at would leave both steps reading
	// different states of main, so the run refuses instead of proving one state and
	// swapping from another.
	if result.Verified.Tip != result.Catchup.Tip {
		return result, fmt.Errorf(
			"doltsquash: the record at the last picked tip read main at %s, but the catch-up stopped at %s, so the run cannot verify and swap at the tip the picks reached",
			result.Verified.Tip, result.Catchup.Tip)
	}
	if result.Verify, err = Verify(ctx, in.Exec, result.Verified, in.Cut); err != nil {
		return result, err
	}
	if !result.Verify.OK() {
		return result, fmt.Errorf("doltsquash: compact-work does not verify against main at %s, so main is untouched: %s",
			result.Verified.Tip, describeDifferences(result.Verify.Differences))
	}
	if result.Swap, err = Swap(ctx, in.Exec, result.Verified.Tip, in.Sleeper); err != nil {
		return result, err
	}
	return result, nil
}

// describeDifferences renders every difference verification found as one line.
func describeDifferences(differences []Difference) string {
	lines := make([]string, len(differences))
	for i, difference := range differences {
		lines[i] = difference.String()
	}
	return strings.Join(lines, "; ")
}
