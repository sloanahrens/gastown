package civol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// pruneKind is the Action kind of a prune, which names a cache-wide command
// rather than one path.
const pruneKind = "prune"

// deferredJobStarted is the Result.Deferred text for a job that started since
// the plan. The planner trims nothing but the build cache while a job runs, so
// an action on any other volume means the plan was made against a quiet runner,
// and its cutoff no longer covers the cache a new job is writing.
const deferredJobStarted = "a job started since the plan"

// deferredPruneJob is the Result.Deferred text for a prune held back because a
// job started while the volumes were trimmed. The trim already removed what the
// plan named, so only the prune is skipped.
const deferredPruneJob = "a job started during the trim; prune skipped"

var (
	// errNoLedger reports a target built without a ledger. Every apply must
	// leave a record, so a run that cannot write one does not start.
	errNoLedger = errors.New("civol apply: no ledger configured")
	// errBadRef reports a plan ref that is not "<volume>:<path>".
	errBadRef = errors.New("ref is not <volume>:<path>")
)

// volumeGroup is the paths one volume's deletes carry, in plan order, with the
// actions they were planned as.
type volumeGroup struct {
	volume  string
	paths   []string
	actions []diskmaint.Action
}

// Apply carries out a plan: it deletes each named path inside the
// docker-in-docker daemon and, when no job is running, prunes the build cache
// and the dangling images. Every action is re-validated first, the job list is
// re-read before any delete, and the run is recorded as one ledger line.
//
// A refused plan deletes nothing. A job that started since the plan defers the
// whole run, and an unreadable job list defers it too: a delete that cannot be
// shown safe does not run.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	var errs []string
	var err error
	if plan.Refused == "" && len(plan.Actions) > 0 {
		if t.cfg.Run == nil {
			errs = append(errs, errNoRunner.Error())
			err = errNoRunner
		} else {
			errs, err = t.trim(ctx, plan.Actions, &result)
		}
	}
	if recErr := t.record(plan, result, errs); recErr != nil {
		err = errors.Join(err, recErr)
	}
	return result, err
}

// trim re-validates the plan's actions, re-reads the job list, removes what the
// plan names one container per volume, and prunes a quiet runner. It returns the
// errors to record and, when the caller cancels, its cancellation.
func (t *Target) trim(ctx context.Context, actions []diskmaint.Action, result *diskmaint.Result) (errs []string, err error) {
	groups, errs := t.validate(actions, result)
	if len(groups) == 0 {
		return errs, nil
	}

	jobs, err := t.jobCount(ctx)
	if err != nil {
		result.Deferred = err.Error()
		errs = append(errs, err.Error())
		return errs, err
	}
	if jobs > 0 && !onlyBuildCache(groups) {
		result.Deferred = deferredJobStarted
		errs = append(errs, deferredJobStarted)
		return errs, nil
	}

	for _, g := range groups {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errs, ctxErr
		}
		lines, stop := t.trimVolume(ctx, g, result)
		errs = append(errs, lines...)
		if stop != nil {
			return errs, stop
		}
	}

	// A prune while a job runs would drop the cache it is using, and an apply
	// that removed nothing has no cache to prune. The job list is read again
	// because a job may have started while the volumes were trimmed.
	if jobs > 0 || len(result.Done) == 0 {
		return errs, nil
	}
	jobs, err = t.jobCount(ctx)
	if err != nil {
		result.Deferred = err.Error()
		errs = append(errs, err.Error())
		return errs, err
	}
	if jobs > 0 {
		result.Deferred = deferredPruneJob
		return errs, nil
	}
	lines, stop := t.prune(ctx, result)
	return append(errs, lines...), stop
}

// validate re-checks the plan's actions and groups the accepted ones by volume.
// An action that fails re-validation is recorded as failed and no command names
// its path; the rest of the plan is still grouped, so one bad ref does not stop
// the trim of a volume the plan named correctly.
func (t *Target) validate(actions []diskmaint.Action, result *diskmaint.Result) (groups []volumeGroup, errs []string) {
	index := make(map[string]int, len(actions))
	for i, action := range actions {
		volume, path, err := t.validateAction(action)
		if err != nil {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: err.Error()})
			errs = append(errs, fmt.Sprintf("action %d: %v", i+1, err))
			continue
		}
		if j, ok := index[volume]; ok {
			groups[j].paths = append(groups[j].paths, path)
			groups[j].actions = append(groups[j].actions, action)
			continue
		}
		index[volume] = len(groups)
		groups = append(groups, volumeGroup{
			volume:  volume,
			paths:   []string{path},
			actions: []diskmaint.Action{action},
		})
	}
	return groups, errs
}

// validateAction re-checks one action before any command names it: the kind the
// planner writes, a "<volume>:<path>" ref for a volume this target is
// configured to trim, a path relative to the volume root, and, for the module
// cache, a whole module directory outside the download cache. The refusal
// quotes its values with %q, so plan text cannot reach a terminal raw.
func (t *Target) validateAction(action diskmaint.Action) (volume, path string, err error) {
	if action.Kind != deleteKind {
		return "", "", fmt.Errorf("action kind %q is not %s", action.Kind, deleteKind)
	}
	volume, path, ok := strings.Cut(action.Ref, ":")
	if !ok {
		return "", "", errBadRef
	}
	if !slices.ContainsFunc(t.cfg.Limits, func(l Limit) bool { return l.Name == volume }) {
		return "", "", fmt.Errorf("volume %q is not a configured limit", volume)
	}
	if unsafePath(path) {
		return "", "", fmt.Errorf("path %q is not a relative path inside %s", path, volume)
	}
	if volume == gomodVolume && (isModuleCache(path) || !isModuleDir(path)) {
		return "", "", fmt.Errorf("path %q is not a module directory outside the module cache", path)
	}
	return volume, path, nil
}

// onlyBuildCache reports a plan whose every action trims the build cache, the
// one volume the planner trims while a job runs.
func onlyBuildCache(groups []volumeGroup) bool {
	for _, g := range groups {
		if g.volume != gocacheVolume {
			return false
		}
	}
	return true
}

// jobCount counts the containers a running job holds inside the
// docker-in-docker container, read again just before any delete so a job that
// started since the plan holds the trim back.
func (t *Target) jobCount(ctx context.Context) (int, error) {
	out, err := t.dind(ctx, "ps", "-q")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, errJobs
	}
	return len(nonEmptyLines(string(out))), nil
}

// trimVolume removes one volume's paths in one throwaway root container. The
// module cache is made writable first, since Go writes its module trees
// read-only. A failed command fails that volume's actions, and the other
// volumes still run.
func (t *Target) trimVolume(ctx context.Context, g volumeGroup, result *diskmaint.Result) (errs []string, stop error) {
	mounts := make([]string, 0, len(g.paths))
	for _, p := range g.paths {
		mounts = append(mounts, volumeRoot+"/"+p)
	}
	if g.volume == gomodVolume {
		args := append([]string{"-R", "u+w", "--"}, mounts...)
		if _, err := t.runInVolume(ctx, g.volume, "chmod", args...); err != nil {
			return failedVolume(result, g, fmt.Errorf("chmod: %w", err)), ctx.Err()
		}
	}
	args := append([]string{"-rf", "--"}, mounts...)
	if _, err := t.runInVolume(ctx, g.volume, "rm", args...); err != nil {
		return failedVolume(result, g, fmt.Errorf("rm: %w", err)), ctx.Err()
	}
	for _, action := range g.actions {
		result.Done = append(result.Done, action)
		result.BytesFreed += action.Bytes
	}
	return nil, nil
}

// runInVolume runs one command in a throwaway root container that mounts the
// volume writable at /c, which is where the trim's paths are named from.
func (t *Target) runInVolume(ctx context.Context, volume, command string, args ...string) ([]byte, error) {
	argv := []string{"run", "--rm", "--user", "0",
		"-v", volume + ":" + volumeRoot, "--entrypoint", command, t.cfg.Image}
	argv = append(argv, args...)
	return t.dind(ctx, argv...)
}

// failedVolume records each of a volume's actions as failed and returns the
// ledger lines that say why.
func failedVolume(result *diskmaint.Result, g volumeGroup, err error) []string {
	for _, action := range g.actions {
		result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: err.Error()})
	}
	return []string{fmt.Sprintf("%s: %v; %d actions recorded as failed", g.volume, err, len(g.actions))}
}

// prune drops the build cache and the dangling images of a quiet runner. Each
// prune is one action, and a prune that fails is recorded as one rather than
// left out. Images are pruned without -a: tagged images another job may pull
// from are never removed here.
func (t *Target) prune(ctx context.Context, result *diskmaint.Result) (errs []string, stop error) {
	prunes := []diskmaint.Action{
		{Kind: pruneKind, Ref: "builder", Why: "build cache; no job is running"},
		{Kind: pruneKind, Ref: "image", Why: "dangling images only; no job is running"},
	}
	for _, action := range prunes {
		if _, err := t.dind(ctx, action.Ref, "prune", "-f"); err != nil {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: action, Err: err.Error()})
			errs = append(errs, fmt.Sprintf("%s prune: %v", action.Ref, err))
			if ctxErr := ctx.Err(); ctxErr != nil {
				return errs, ctxErr
			}
			continue
		}
		result.Done = append(result.Done, action)
	}
	return errs, nil
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
