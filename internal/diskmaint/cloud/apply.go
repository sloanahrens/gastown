package cloud

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

// Compile-time check that the cloud target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// The Mode of the one ledger line a run appends.
const (
	modeApply  = "apply"
	modeDryRun = "dry-run"
)

var (
	// errBadRef reports a plan action whose ref is not an image id.
	errBadRef = errors.New("the ref is not an image id")
	// errPlanRefused reports an apply of a refused plan. Nothing is removed, and
	// the refusal is an error as well as a ledger line, so a direct caller does
	// not read a run that decided nothing as success.
	errPlanRefused = errors.New("cloud apply: the plan is refused; nothing was removed")
)

// removeFailed is the Failure reason for a removal that did not succeed. The
// runner's error carries the command's stderr, so the record names the command
// and never quotes what docker printed.
const removeFailed = "docker image rm failed; the image is still present"

// Apply carries out a plan's removals and records the run as one ledger line.
//
// It removes only when the plan's own policy asks for an apply: a plan with
// Policy.Apply false is the dry run and is the default, and Apply then runs no
// docker command at all and records the run as mode dry-run. Every removal names
// one image id — docker's prune is never run, and removing by id is what drops
// every tag of the image the plan decided about. The removal is forced because
// docker refuses to delete an id that several tags name without it, and no keep
// rule spared a tag of the id. --no-prune keeps a removal to that image alone:
// without it docker also deletes an untagged parent image, and every row this
// plan does not name as a removal is one it keeps.
//
// One failed removal does not stop the others: it becomes a Failure, the run
// continues, and the ledger line counts what was done. A refused plan removes
// nothing and returns errPlanRefused after recording the refusal.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	mode := modeDryRun
	if plan.Policy.Apply {
		mode = modeApply
	}
	var errs []string
	var err error
	switch {
	case !plan.Policy.Apply:
		// The dry run: the plan is the answer, and nothing is removed.
	case plan.Refused != "":
		err = errPlanRefused
		errs = append(errs, err.Error())
	default:
		errs = t.removePlanned(ctx, plan.Actions, &result)
	}
	if recErr := t.record(plan, mode, result, errs); recErr != nil {
		err = errors.Join(err, recErr)
	}
	return result, err
}

// removePlanned removes each planned image and returns the reasons to record.
// An action is validated before its command runs, so no ref but an image id
// reaches docker's argv; a failed removal is recorded and the rest go ahead.
func (t *Target) removePlanned(ctx context.Context, actions []diskmaint.Action, result *diskmaint.Result) []string {
	var errs []string
	for i, a := range actions {
		id, err := actionID(a)
		if err != nil {
			// A ref that fails validation is plan text nobody has checked, so
			// the record names the action's position and not its ref.
			result.Failed = append(result.Failed, diskmaint.Failure{Action: a, Err: err.Error()})
			errs = append(errs, fmt.Sprintf("action %d: %v", i+1, err))
			continue
		}
		if _, err := t.client().Docker(ctx, "image", "rm", "--no-prune", "--force", id); err != nil {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: a, Err: removeFailed})
			errs = append(errs, fmt.Sprintf("remove %s: %s", id, removeFailed))
			continue
		}
		result.Done = append(result.Done, a)
		result.BytesFreed += a.Bytes
	}
	return errs
}

// actionID validates one plan action and returns the image id it names.
func actionID(a diskmaint.Action) (string, error) {
	if a.Kind != actionRemove {
		return "", fmt.Errorf("action kind is not %s", actionRemove)
	}
	if !registry.ValidDigest(a.Ref) {
		return "", errBadRef
	}
	return a.Ref, nil
}

// record appends the run's one ledger line.
func (t *Target) record(plan diskmaint.Plan, mode string, result diskmaint.Result, errs []string) error {
	err := t.cfg.Ledger.Append(diskmaint.LedgerEntry{
		TS:         time.Now().UTC(),
		Target:     planTarget,
		Mode:       mode,
		Planned:    len(plan.Actions),
		Done:       len(result.Done),
		BytesFreed: result.BytesFreed,
		Refused:    plan.Refused,
		Deferred:   result.Deferred,
		Errors:     errs,
	})
	if err != nil {
		return fmt.Errorf("record the apply: %w", err)
	}
	return nil
}
