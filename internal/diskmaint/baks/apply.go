package baks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

var (
	// errNoLedger reports a target built without a ledger. Every apply must
	// leave a record, so a run that cannot write one does not start.
	errNoLedger = errors.New("baks apply: no ledger configured")
	// errDryRun reports an Apply whose policy did not allow it. A dry run is a
	// Plan, so a caller that reaches Apply with Apply false is a mistake, and
	// nothing is removed.
	errDryRun = errors.New("baks apply: the policy is a dry run, so nothing was removed")
	// errPlanRefused reports a plan that could not read its inputs. Nothing is
	// removed, and the refusal is an error rather than an empty success.
	errPlanRefused = errors.New("baks apply: the plan is refused, so nothing was removed")
	// errUnknownKind reports an action this target does not carry.
	errUnknownKind = errors.New("action kind is not delete-file")
	// errBadRef reports a ref that is not a clean path, one with a .. element,
	// a doubled separator or a trailing slash among them. The ref is not quoted:
	// it is plan text no check has passed.
	errBadRef = errors.New("ref is not a clean path to a file")
	// errOutsideDirs reports a file that is not directly inside one of the
	// configured directories. Discovery only lists direct children, so anything
	// else is not a file this target may remove.
	errOutsideDirs = errors.New("file is not directly inside a configured directory")
	// errNotBackup reports a name that is neither a .bak copy nor a bd.<sha>
	// build.
	errNotBackup = errors.New("name is neither .bak nor bd.<sha>")
	// errNotRegular reports a symlink, a directory or another non-regular file.
	// Lstat decides, so a link is refused rather than followed to what it names.
	errNotRegular = errors.New("not a regular file")
)

// Apply removes the files the plan names. Each one is checked again before it
// is removed, and a file that fails a check or a removal is recorded as a
// Failure while the rest still run. The run is recorded as one ledger line.
//
// A dry run and a refused plan remove nothing and report that as an error, so
// an unapplied plan cannot read as a clean run. An empty plan removes nothing
// and reads as clean. A canceled context stops the run between files.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	var errs []string
	var err error
	switch {
	case !t.cfg.Policy.Apply:
		err = errDryRun
		errs = append(errs, err.Error())
	case plan.Refused != "":
		err = errPlanRefused
		errs = append(errs, err.Error())
	default:
		errs, err = t.removeAll(ctx, plan.Actions, &result)
	}
	if recErr := t.record(plan, result, errs); recErr != nil {
		err = errors.Join(err, recErr)
	}
	return result, err
}

// removeAll removes the planned files in order and returns the errors to
// record. One file that fails does not stop the run. A canceled context does,
// and its error is returned.
func (t *Target) removeAll(ctx context.Context, actions []diskmaint.Action, result *diskmaint.Result) ([]string, error) {
	var errs []string
	for i, action := range actions {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err.Error())
			return errs, err
		}
		if err := t.removeOne(action); err != nil {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: err.Error()})
			// The ref is plan text no check has passed, so the record names the
			// action's position rather than quoting it.
			errs = append(errs, fmt.Sprintf("action %d: %v", i+1, err))
			continue
		}
		result.Done = append(result.Done, action)
		result.BytesFreed += action.Bytes
	}
	return errs, nil
}

// removeOne re-checks one planned file and removes it. Every check happens
// before the removal, and an error from any of them means nothing was removed.
func (t *Target) removeOne(action diskmaint.Action) error {
	if action.Kind != deleteKind {
		return errUnknownKind
	}
	path, err := t.plannedPath(action.Ref)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("the file state cannot be read: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errNotRegular
	}
	if err := t.remove(path); err != nil {
		return fmt.Errorf("remove failed: %w", err)
	}
	return nil
}

// plannedPath returns ref when it is a clean path to a backup file directly
// inside one of the configured directories. Its name must be a candidate, the
// same name test discovery applies.
func (t *Target) plannedPath(ref string) (string, error) {
	if ref == "" || filepath.Clean(ref) != ref {
		return "", errBadRef
	}
	if !t.configured(filepath.Dir(ref)) {
		return "", errOutsideDirs
	}
	if !candidate(filepath.Base(ref)) {
		return "", errNotBackup
	}
	return ref, nil
}

// configured reports whether dir is one of the directories the target reads.
// Both lists are compared cleaned, the way discovery reads them.
func (t *Target) configured(dir string) bool {
	for _, list := range [][]string{t.cfg.ConfigDirs, t.cfg.BinaryDirs} {
		for _, d := range list {
			if d != "" && filepath.Clean(d) == dir {
				return true
			}
		}
	}
	return false
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
		Errors:     errs,
	})
	if err != nil {
		return fmt.Errorf("record the apply: %w", err)
	}
	return nil
}
