package worktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

const (
	// kindRemove is the Action kind that removes a worktree; its Ref is the path.
	kindRemove = "remove-worktree"
	// kindDeleteBranch is the Action kind that deletes a landed worktree's local
	// branch; its Ref is "<path>#<branch>".
	kindDeleteBranch = "delete-branch"
)

// branchName is the only shape of branch name a command may receive.
var branchName = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

var (
	// errNoLedger reports a target built without a ledger. Every apply must
	// leave a record, so a run that cannot write one does not start.
	errNoLedger = errors.New("worktree apply: no ledger configured")
	// errNotOverseer reports a path that is not an overseer-* directory directly
	// under <Town>/<rig>/crew for a configured rig. It does not quote the path:
	// the path is plan text no check has passed.
	errNotOverseer = errors.New("path is not an overseer-* checkout under <Town>/<rig>/crew")
	// errBadRef reports a delete-branch ref that is not "<path>#<branch>".
	errBadRef = errors.New("ref is not <path>#<branch>")
	// errBadBranch reports a branch name that fails branchName, or starts with a
	// dash that git would read as an option.
	errBadBranch = errors.New("branch name is not a safe git branch name")
	// errCommonDir reports a failed or empty read of the shared git dir, which
	// the removal needs before it can run.
	errCommonDir = errors.New("git rev-parse --git-common-dir gave no git dir")
	// errRemoveFailed reports a refused or failed worktree removal. Git's output
	// is not echoed: it can name a path or hold a secret.
	errRemoveFailed = errors.New("git worktree remove failed")
	// errBranchFailed reports a refused or failed branch delete.
	errBranchFailed = errors.New("git branch -D failed")
	// errBranchKept reports a branch whose worktree was not removed in this run,
	// so the branch stays.
	errBranchKept = errors.New("the worktree was not removed, so its branch is kept")
	// errUnknownKind reports an action this target does not carry.
	errUnknownKind = errors.New("action kind is not remove-worktree or delete-branch")
)

// Apply removes what the plan names: each overseer-* worktree with git worktree
// remove, never with --force, and then the local branch of a landed worktree
// once its removal has succeeded. Every path and branch is re-validated before a
// command names it. One failed action is recorded and the rest still run, and
// the run is recorded as one ledger line.
//
// A refused plan, and an empty one, run no command. A redirected git environment
// refuses the run, as it refuses a plan. Apply has no Policy: a dry run is a
// Plan, and the caller calls Apply only when it means to apply.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	var errs []string
	var err error
	if plan.Refused == "" && len(plan.Actions) > 0 {
		errs, err = t.runPlan(ctx, plan.Actions, &result)
	}
	if recErr := t.record(plan, result, errs); recErr != nil {
		err = errors.Join(err, recErr)
	}
	return result, err
}

// runPlan carries out the actions in order and returns the errors to record and,
// when the caller cancels, its cancellation.
func (t *Target) runPlan(ctx context.Context, actions []diskmaint.Action, result *diskmaint.Result) ([]string, error) {
	if t.cfg.Run == nil {
		return []string{errNoRunner.Error()}, errNoRunner
	}
	if names := redirectedBy(t.environ()); len(names) > 0 {
		err := fmt.Errorf("%w: %s", errRedirected, strings.Join(names, ", "))
		return []string{err.Error()}, err
	}

	// removed maps each worktree this run removed to the git dir its branch
	// lives in. The dir is read before the removal, because the worktree is
	// gone after it. A branch is deleted only through an entry here.
	removed := map[string]string{}
	var errs []string
	for _, action := range actions {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err.Error())
			return errs, err
		}
		switch action.Kind {
		case kindRemove:
			path, common, err := t.removeWorktree(ctx, action.Ref)
			if err != nil {
				fail(result, action, err)
				errs = append(errs, "remove "+action.Ref+": "+err.Error())
				continue
			}
			removed[path] = common
			result.Done = append(result.Done, action)
		case kindDeleteBranch:
			if err := t.deleteBranch(ctx, action.Ref, removed); err != nil {
				fail(result, action, err)
				errs = append(errs, "branch "+action.Ref+": "+err.Error())
				continue
			}
			result.Done = append(result.Done, action)
		default:
			fail(result, action, errUnknownKind)
			errs = append(errs, "action: "+errUnknownKind.Error())
		}
	}
	return errs, nil
}

// removeWorktree removes one overseer-* worktree. It returns the cleaned path
// and the git dir its branch lives in. The removal runs without --force: git
// refuses a dirty worktree, and that refusal is the action's failure. A refused
// removal is not retried.
func (t *Target) removeWorktree(ctx context.Context, ref string) (string, string, error) {
	path, err := overseerPath(t.cfg.Town, t.cfg.Rigs, ref)
	if err != nil {
		return "", "", err
	}
	out, err := t.git(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", "", errCommonDir
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", "", errCommonDir
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	if _, err := t.cfg.Run(ctx, nil, "git", "--git-dir", common, "worktree", "remove", path); err != nil {
		return "", "", errRemoveFailed
	}
	return path, common, nil
}

// deleteBranch deletes the branch named by a delete-branch ref, but only when
// the worktree it was paired with was removed in this run. The branch name is
// checked before it reaches a command.
func (t *Target) deleteBranch(ctx context.Context, ref string, removed map[string]string) error {
	i := strings.LastIndex(ref, "#")
	if i < 0 {
		return errBadRef
	}
	path, branch := filepath.Clean(ref[:i]), ref[i+1:]
	if !branchName.MatchString(branch) || strings.HasPrefix(branch, "-") {
		return errBadBranch
	}
	common, ok := removed[path]
	if !ok {
		return errBranchKept
	}
	if _, err := t.cfg.Run(ctx, nil, "git", "--git-dir", common, "branch", "-D", branch); err != nil {
		return errBranchFailed
	}
	return nil
}

// overseerPath returns ref cleaned when it is a path directly under
// <town>/<rig>/crew, for one of rigs, whose base name starts with overseer-. The
// clone, a directory under any other parent, and a path nested deeper are all
// refused, so no command names them.
func overseerPath(town string, rigs []string, ref string) (string, error) {
	path := filepath.Clean(ref)
	if !strings.HasPrefix(filepath.Base(path), removePrefix) {
		return "", errNotOverseer
	}
	crew := filepath.Dir(path)
	if filepath.Base(crew) != crewDir {
		return "", errNotOverseer
	}
	rigDir := filepath.Dir(crew)
	if filepath.Dir(rigDir) != filepath.Clean(town) || !slices.Contains(rigs, filepath.Base(rigDir)) {
		return "", errNotOverseer
	}
	return path, nil
}

// fail records one action that did not complete.
func fail(result *diskmaint.Result, action diskmaint.Action, err error) {
	result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: err.Error()})
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
