package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

// Compile-time check that the cloud target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// The Mode of the one ledger line a run appends.
const (
	modeApply  = "apply"
	modeDryRun = "dry-run"
)

// builderKeep is the build cache an apply leaves: docker builder prune deletes
// the cache beyond it, so a later build still finds its newest layers. It is a
// constant, so no caller can widen the prune to the whole cache, and the prune
// is never the full --all one.
const builderKeep = "8GB"

// actionPruneBuildCache is the Action kind of the build cache prune. It is not
// a plan action: Apply adds it to the result it reports, so a dry run never
// names it and an apply's ledger line counts it beside the images.
const actionPruneBuildCache = "prune-build-cache"

var (
	// errBadRef reports a plan action whose ref is not an image id.
	errBadRef = errors.New("the ref is not an image id")
	// errPlanRefused reports an apply of a refused plan. Nothing is removed, and
	// the refusal is an error as well as a ledger line, so a direct caller does
	// not read a run that decided nothing as success.
	errPlanRefused = errors.New("cloud apply: the plan is refused; nothing was removed")
	// errNothingRemoved reports an apply that removed nothing and had a step
	// fail: every image the plan named stayed, or the plan named none and the
	// build cache prune failed. A run that did nothing it meant to is not a
	// success a caller may read from a nil error; Result.Failed says what
	// stayed. One failure among successes is not this error: the run went on,
	// and Failed carries it.
	errNothingRemoved = errors.New("cloud apply: every removal failed; nothing was removed")
)

// removeFailed is the Failure reason for a removal that did not succeed. The
// runner's error carries the command's stderr, so the record names the command
// and never quotes what docker printed.
const removeFailed = "docker image rm failed; the image is still present"

// The reasons a build cache prune is recorded with, worded the same way: the
// runner's error carries the command's stderr, so neither quotes what docker
// printed.
const (
	pruneFailed = "docker builder prune failed; the build cache may be over its keep size"
	pruneUnread = "docker builder prune printed no reclaimed total; the bytes it freed are unknown"
)

// Apply carries out a plan's removals, prunes the daemon's build cache, and
// records the run as one ledger line.
//
// It removes only when the plan's own policy asks for an apply: a plan with
// Policy.Apply false is the dry run and is the default, and Apply then runs no
// docker command at all and records the run as mode dry-run. Every removal names
// one image id — docker's image prune is never run, and removing by id is what
// drops every tag of the image the plan decided about. The removal is forced
// because docker refuses to delete an id that several tags name without it, and
// no keep rule spared a tag of the id. --no-prune keeps a removal to that image
// alone: without it docker also deletes an untagged parent image, and every row
// this plan does not name as a removal is one it keeps.
//
// An apply ends by bringing the build cache back under builderKeep with one
// docker builder prune --keep-storage <builderKeep> --force, never a full prune,
// so the cache a later build reuses stays. The prune is a step of the apply and
// not one of the plan's actions: the ledger line counts it in Done beside the
// images and adds the space it reports reclaimed to BytesFreed. A prune that
// fails, or that prints no reclaimed total, is recorded in the line's errors and
// the run goes on, since what the plan removed is already gone. A refused plan
// runs no docker command, the prune included: a refusal records that nothing was
// changed.
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
		// The images removed, counted before the prune: a successful prune is
		// not an image the plan meant to remove, and it must not hide a run in
		// which every removal failed.
		removed := len(result.Done)
		errs = append(errs, t.pruneBuildCache(ctx, &result)...)
		if removed == 0 && len(result.Failed) > 0 {
			err = errNothingRemoved
		}
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

// pruneBuildCache brings the daemon's build cache back under builderKeep with
// the one build cache prune this target runs, over the daemon the target's
// removals already go to. --keep-storage names the size to keep and --all is
// never passed, so the newest cache entries survive; --force keeps the prune
// from waiting for a confirmation nobody is there to give.
//
// The prune is recorded as done with the bytes docker reports it reclaimed, so
// the run's ledger line counts it. A prune that fails is a Failure and a reason
// for the line. One that ran but printed no total is recorded as done with the
// bytes left unknown, and the reason says so rather than the line reporting a
// prune that freed nothing. Either way the run goes on.
func (t *Target) pruneBuildCache(ctx context.Context, result *diskmaint.Result) []string {
	action := diskmaint.Action{
		Kind: actionPruneBuildCache,
		Ref:  "builder",
		Why:  "the build cache beyond --keep-storage " + builderKeep,
	}
	out, err := t.client().Docker(ctx, "builder", "prune", "--keep-storage", builderKeep, "--force")
	if err != nil {
		result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: pruneFailed})
		return []string{pruneFailed}
	}
	freed, ok := pruneTotal(out)
	if ok {
		action.Bytes = freed
		result.BytesFreed += freed
	}
	result.Done = append(result.Done, action)
	if !ok {
		return []string{pruneUnread}
	}
	return nil
}

// pruneTotal reads the bytes a build cache prune reports it reclaimed. Docker
// prints the figure as the last line of its output — "Total reclaimed space:
// 1.2GB" for the classic builder, "Total: 1.2GB" for the buildx one — so the
// line must name a total and carry a size ParseSize reads after a colon. ok is
// false for output without one, so a caller never reads an unknown as a prune
// that freed nothing.
func pruneTotal(out []byte) (n int64, ok bool) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.Contains(last, "Total") {
		return 0, false
	}
	i := strings.LastIndex(last, ":")
	if i < 0 {
		return 0, false
	}
	size := last[i+1:]
	n, err := dockerimg.ParseSize(strings.TrimSpace(size))
	if err != nil {
		return 0, false
	}
	return n, true
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
