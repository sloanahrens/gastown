package daemon

// The Forgejo Actions run retention step. The daily window's pass runs it after
// its targets: per landing repository it lists every workflow run, plans the
// deletes, and in apply mode removes them with the landing bot's token. The
// runs table otherwise grows without bound, and an unfiltered listing on a
// repository with thousands of runs takes tens of seconds (gt-573420).
//
// A run is kept when it is among the newest keep_newest of its repository, when
// it is younger than days, when it is not finished (waiting, running, blocked,
// or any status the step does not know), and when its branch is the green/ ref
// of the installed image pair. Everything else is deleted, oldest first.
//
// The plan is built from the whole listing before any delete, so a delete never
// shifts the pages being read. A listing that fails or cannot be read refuses
// the repository's plan: a partial listing would name the wrong newest runs.
// Enabled false, or a patrol mode other than apply, logs the plan and deletes
// nothing.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
)

const (
	// actionsRetentionPageSize is the page the listing asks for: the most a
	// Forgejo listing answers with.
	actionsRetentionPageSize = 50

	// actionsRetentionMaxPages bounds the listing. A repository past it is
	// refused, not cut short, since a cut listing would name the wrong newest runs.
	actionsRetentionMaxPages = 1000

	// actionsRetentionMaxDeletes bounds the deletes one pass makes in one
	// repository. The rest are planned again by the next pass.
	actionsRetentionMaxDeletes = 1000

	// actionsRetentionRunTimeout bounds one pass over every repository. A pass
	// still running after it is stuck, not slow.
	actionsRetentionRunTimeout = 30 * time.Minute

	// actionsRetentionShow is how many of the oldest planned runs a dry run names.
	actionsRetentionShow = 10
)

// actionsRetentionFinished are the statuses of a run that has ended. Any other
// status is kept.
var actionsRetentionFinished = []string{deployRunSucceeded, deployRunFailed, deployRunCancelled, "skipped"}

// actionsRunAPI is the part of the Forgejo client the retention step uses. The
// production client satisfies it; a test passes one over an httptest server.
type actionsRunAPI interface {
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	DeleteRun(ctx context.Context, owner, repo string, runID int64) error
}

// actionsRetentionRepo is one repository the step plans for: the rig it serves,
// the repository's owner and name, and the client that reads and deletes its runs.
type actionsRetentionRepo struct {
	Rig   string
	Owner string
	Repo  string
	API   actionsRunAPI
}

// actionsRetentionPlan is what a repository's pass would delete. Delete is
// oldest first, and Kept counts the runs the plan keeps.
type actionsRetentionPlan struct {
	Delete []forgejo.ActionRun
	Kept   int
}

// planActionsRetention decides which of runs to delete. runs may hold a run
// twice when pages shift during the listing, so it is keyed by ID; the newest
// by ID is the newest run. A run beyond the keep whose created time does not
// parse refuses the whole plan, since a run the step cannot age is not one it
// may delete.
func planActionsRetention(runs []forgejo.ActionRun, now time.Time, days, keep int, protectedRef string) (actionsRetentionPlan, error) {
	byID := make(map[int64]forgejo.ActionRun, len(runs))
	for _, r := range runs {
		byID[r.ID] = r
	}
	ids := slices.Sorted(maps.Keys(byID))
	slices.Reverse(ids)

	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	var plan actionsRetentionPlan
	for i, id := range ids {
		run := byID[id]
		if i < keep {
			plan.Kept++
			continue
		}
		created, err := time.Parse(time.RFC3339, run.Created)
		if err != nil {
			return actionsRetentionPlan{}, fmt.Errorf("run %d has created time %q, which is not RFC3339", id, run.Created)
		}
		switch {
		case !created.Before(cutoff), !slices.Contains(actionsRetentionFinished, run.Status), run.PrettyRef == protectedRef:
			plan.Kept++
		default:
			plan.Delete = append(plan.Delete, run)
		}
	}
	slices.Reverse(plan.Delete)
	return plan, nil
}

// listActionRuns reads every page of a repository's runs. An error on any page
// fails the whole listing, and a listing that runs past actionsRetentionMaxPages
// is refused.
func listActionRuns(ctx context.Context, api actionsRunAPI, owner, repo string) ([]forgejo.ActionRun, error) {
	return listActionRunsPaged(ctx, api, owner, repo, actionsRetentionMaxPages)
}

// listActionRunsPaged is listActionRuns with the page cap as a parameter, so a
// test can reach the refusal without serving every page the cap allows.
func listActionRunsPaged(ctx context.Context, api actionsRunAPI, owner, repo string, maxPages int) ([]forgejo.ActionRun, error) {
	var all []forgejo.ActionRun
	for page := 1; page <= maxPages; page++ {
		list, err := api.ListRuns(ctx, owner, repo, forgejo.RunFilter{Page: page, Limit: actionsRetentionPageSize})
		if err != nil {
			return nil, err
		}
		all = append(all, list.Runs...)
		if len(list.Runs) < actionsRetentionPageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("the listing ran past %d pages; it is refused rather than cut short", maxPages)
}

// installedGreenRef is the green/ ref of the installed image pair, the one run
// the retention step never deletes. An unreadable values file, or none, refuses
// the step: with no pair named, nothing can be proven unreferenced.
func installedGreenRef(townRoot string) (string, error) {
	v, ok, err := imagevalues.LoadIn(filepath.Join(townRoot, "gastown"))
	if err != nil {
		return "", fmt.Errorf("the installed image pair could not be read: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("no installed image pair is recorded in %s, so no green ref is protected", imagevalues.DefaultRelPath)
	}
	return greenRefName(v.Commit), nil
}

// actionsRetentionRepos resolves the repositories the step plans for: each rig
// the landing worker serves whose merge_queue.forgejo block resolves, read
// through the landing bot's token. A rig that names no block is skipped with a
// line, since it has no repository whose runs the step may prune. A block the
// run cannot read, or a remote that does not resolve, refuses the whole list,
// rather than planning the rest and leaving that rig's runs out of the pass
// without a line.
func (d *Daemon) actionsRetentionRepos() ([]actionsRetentionRepo, error) {
	tokenDir, err := d.diskMaintenanceSeams().actionsTokenDir()
	if err != nil {
		return nil, fmt.Errorf("the landing bot token directory could not be resolved: %w", err)
	}
	var out []actionsRetentionRepo
	for _, name := range landingWorkerRigs(d.patrol(), d.getKnownRigs()) {
		fj, err := rig.ResolveForgejoConfigWithError(d.repoFileOpener(), d.config.TownRoot, name)
		if err != nil {
			return nil, fmt.Errorf("rig %s: its merge_queue.forgejo settings could not be read: %w", name, err)
		}
		if fj == nil {
			d.logger.Printf("disk_maintenance: actions_retention: %s: skipped, it names no merge_queue.forgejo block", name)
			continue
		}
		// Both parsers refuse a remote without a scheme and host, and their errors
		// quote the URL, which can carry a credential. The refusal therefore names
		// the setting and not the error text.
		owner, repo, err := land.RepoFromRemoteURL(fj.RemoteURL)
		base, baseErr := land.APIBaseFromRemoteURL(fj.RemoteURL)
		if err != nil || baseErr != nil {
			return nil, fmt.Errorf("rig %s: its merge_queue.forgejo remote_url does not name a repository", name)
		}
		api, err := forgejo.NewClient(agentconfig.ForgejoRoleLanding,
			forgejo.WithBaseURL(base),
			forgejo.WithTokenFile(filepath.Join(tokenDir, "forgejo-"+agentconfig.ForgejoRoleLanding+".env")))
		if err != nil {
			return nil, fmt.Errorf("rig %s: the %s bot token is unusable: %w", name, agentconfig.ForgejoRoleLanding, err)
		}
		out = append(out, actionsRetentionRepo{Rig: name, Owner: owner, Repo: repo, API: api})
	}
	return out, nil
}

// runActionsRetention runs the step over every repository the landing worker
// serves. It is a no-op unless the block is set, and a setting it cannot act on
// refuses the step. It deletes only when the block is enabled and patrolApply,
// the patrol's own mode, is apply; otherwise it logs the plan.
func (d *Daemon) runActionsRetention(now time.Time, patrolApply bool) {
	cfg := diskMaintenanceSettings(d.patrol())
	if cfg == nil || cfg.ActionsRetention == nil {
		return
	}
	apply := patrolApply && cfg.ActionsRetention.Enabled
	days, err := cfg.ActionsRetention.RetentionDays()
	if err != nil {
		d.logger.Printf("disk_maintenance: actions_retention: not running: %v", err)
		return
	}
	keep, err := cfg.ActionsRetention.RetentionKeepNewest()
	if err != nil {
		d.logger.Printf("disk_maintenance: actions_retention: not running: %v", err)
		return
	}
	protected, err := installedGreenRef(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("disk_maintenance: actions_retention: not running: %v", err)
		return
	}
	s := d.diskMaintenanceSeams()
	repos, err := s.actionsRepos()
	if err != nil {
		d.logger.Printf("disk_maintenance: actions_retention: not running: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(d.actionsRetentionParent(), actionsRetentionRunTimeout)
	defer cancel()
	for _, repo := range repos {
		d.retainActionsRuns(ctx, repo, now, days, keep, protected, apply)
	}
}

// actionsRetentionParent is the context the step's own timeout derives from:
// the daemon's, so a shutdown stops the step, and background when there is none.
func (d *Daemon) actionsRetentionParent() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// retainActionsRuns plans one repository's deletes and, when apply is set,
// makes them. Each delete that fails is counted and the rest still run; the
// first failure's cause is logged, with the status code and no response body.
func (d *Daemon) retainActionsRuns(ctx context.Context, repo actionsRetentionRepo, now time.Time, days, keep int, protected string, apply bool) {
	runs, err := listActionRuns(ctx, repo.API, repo.Owner, repo.Repo)
	if err != nil {
		d.logger.Printf("disk_maintenance: actions_retention: %s: refused, the runs could not be listed: %s", repo.Rig, actionsErrText(err))
		return
	}
	plan, err := planActionsRetention(runs, now, days, keep, protected)
	if err != nil {
		d.logger.Printf("disk_maintenance: actions_retention: %s: refused: %v", repo.Rig, err)
		return
	}
	if !apply {
		d.logger.Printf("disk_maintenance: actions_retention: %s dry_run: %d to delete, %d kept (oldest: %s)",
			repo.Rig, len(plan.Delete), plan.Kept, actionsRetentionShowList(plan.Delete))
		return
	}
	var deleted, failed int
	var firstFailure error
	for i, run := range plan.Delete {
		if i >= actionsRetentionMaxDeletes || ctx.Err() != nil {
			break
		}
		if err := repo.API.DeleteRun(ctx, repo.Owner, repo.Repo, run.ID); err != nil {
			failed++
			if firstFailure == nil {
				firstFailure = err
			}
			continue
		}
		deleted++
	}
	if firstFailure != nil {
		d.logger.Printf("disk_maintenance: actions_retention: %s: %d of %d deletes failed; the first: %s",
			repo.Rig, failed, deleted+failed, actionsErrText(firstFailure))
	}
	d.logger.Printf("disk_maintenance: actions_retention: %s apply: deleted %d of %d planned, %d kept",
		repo.Rig, deleted, len(plan.Delete), plan.Kept)
}

// actionsRetentionShowList names the oldest planned runs for a dry run's line.
func actionsRetentionShowList(runs []forgejo.ActionRun) string {
	if len(runs) == 0 {
		return "none"
	}
	shown := runs
	if len(shown) > actionsRetentionShow {
		shown = shown[:actionsRetentionShow]
	}
	parts := make([]string, 0, len(shown))
	for _, r := range shown {
		parts = append(parts, fmt.Sprintf("#%d %s %s", r.ID, r.Created, r.Status))
	}
	return strings.Join(parts, ", ")
}

// actionsErrText is the cause a log line may carry: the status code of a Forgejo
// refusal, never its response body, which can echo request content.
func actionsErrText(err error) string {
	var apiErr *forgejo.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("forgejo answered %d", apiErr.StatusCode)
	}
	return err.Error()
}
