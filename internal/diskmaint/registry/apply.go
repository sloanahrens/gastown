package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// ErrDeferred reports that an apply left the registry alone because another run
// holds the lock that serializes deletes with pushes. The registry package does
// not import polecatimage, so the caller wraps polecatimage.ErrLockHeld in this
// sentinel, and Apply reads any error from Config.Lock that wraps it as a
// deferral rather than a failure.
var ErrDeferred = errors.New("registry apply deferred")

// errNoLedger reports a target built without a ledger. Every apply must leave a
// record, so a run that cannot write one does not start.
var errNoLedger = errors.New("registry apply: no ledger configured")

// errNotRun reports a Config.Lock that reported success without running the
// deletes it was given: the run did nothing, so it must not read as an apply.
var errNotRun = errors.New("registry apply: the lock returned without running the deletes")

// actionDelete is the only action kind a plan for this target carries; the
// planner writes it and Apply refuses anything else.
const actionDelete = "delete-manifest"

// Apply carries out a plan: it deletes each named manifest by digest, keeps
// going when one delete fails, and records the run as one ledger line.
//
// The deletes run inside Config.Lock — the lock every push holds — so a delete
// cannot race a push. A lock another run holds defers the whole run: nothing is
// deleted, Result.Deferred says why, and the ledger line records the deferral.
// Config.Lock nil means the caller already holds the lock.
//
// A refused plan, and an empty one, have nothing to delete: Apply records that
// and returns before taking the lock.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	if plan.Refused != "" || len(plan.Actions) == 0 {
		return result, t.record(plan, result, nil)
	}

	var errs []string
	var stopped error
	ran := false
	body := func(ctx context.Context) error {
		ran = true
		errs, stopped = t.deleteAll(ctx, plan.Actions, &result)
		errs = append(errs, t.gc(ctx, &result)...)
		return nil
	}

	// The body's outcome is recorded whatever the lock does afterwards: a
	// release that fails must not hide the deletes that already happened.
	lockErr := t.locked(ctx, body)
	err := stopped
	switch {
	case lockErr == nil && ran:
	case !ran && errors.Is(lockErr, ErrDeferred):
		result.Deferred = lockErr.Error()
	case !ran && lockErr == nil:
		errs = append(errs, errNotRun.Error())
		err = errNotRun
	default:
		errs = append(errs, "lock: "+lockErr.Error())
		err = errors.Join(err, lockErr)
	}
	if recErr := t.record(plan, result, errs); recErr != nil {
		err = errors.Join(err, recErr)
	}
	return result, err
}

// locked runs fn through Config.Lock, or directly when the caller already holds
// the lock.
func (t *Target) locked(ctx context.Context, fn func(context.Context) error) error {
	if t.cfg.Lock == nil {
		return fn(ctx)
	}
	return t.cfg.Lock(ctx, fn)
}

// deleteAll applies the actions in order and returns the errors to record. One
// failed delete does not stop the run: it becomes a Failure and the rest are
// attempted. ErrDeletesDisabled is the exception — the registry turned deletes
// off, so the actions after it are counted and the run stops.
func (t *Target) deleteAll(ctx context.Context, actions []diskmaint.Action, result *diskmaint.Result) (errs []string, stopped error) {
	for i, action := range actions {
		repo, digest, err := actionRef(action)
		if err != nil {
			// A ref that fails validation is plan text nobody checked, so the
			// record names the action's position rather than quoting it.
			result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: err.Error()})
			errs = append(errs, fmt.Sprintf("action %d: %v", i+1, err))
			continue
		}
		if err := t.client.DeleteManifest(ctx, repo, digest); err != nil {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: err.Error()})
			errs = append(errs, "delete "+action.Ref+": "+err.Error())
			if errors.Is(err, ErrDeletesDisabled) {
				errs = append(errs, fmt.Sprintf("deletes disabled: %d of %d actions not attempted", len(actions)-i-1, len(actions)))
				return errs, fmt.Errorf("delete %s: %w", action.Ref, ErrDeletesDisabled)
			}
			continue
		}
		result.Done = append(result.Done, action)
		result.BytesFreed += action.Bytes
	}
	return errs, nil
}

// actionRef splits an action's "repo@sha256:..." ref and validates both halves
// before either reaches a URL path, so a name the registry should never hear
// fails here and sends no request. The refusal does not quote the ref: it is
// plan text no validator has passed.
func actionRef(action diskmaint.Action) (repo, digest string, err error) {
	if action.Kind != actionDelete {
		return "", "", fmt.Errorf("action kind %q is not %s", action.Kind, actionDelete)
	}
	repo, digest, ok := strings.Cut(action.Ref, "@")
	if !ok || !ValidRepo(repo) || !ValidDigest(digest) {
		return "", "", fmt.Errorf("ref is not repo@sha256:<64 hex>: %w", ErrBadName)
	}
	return repo, digest, nil
}

// record appends the run's one ledger line.
func (t *Target) record(plan diskmaint.Plan, result diskmaint.Result, errs []string) error {
	err := t.cfg.Ledger.Append(diskmaint.LedgerEntry{
		TS:         time.Now().UTC(),
		Target:     planTarget,
		Mode:       "apply",
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
