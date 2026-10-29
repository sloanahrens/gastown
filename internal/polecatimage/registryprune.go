package polecatimage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

// The modes POLECAT_IMAGE_REGISTRY_PRUNE accepts. A town that names none keeps
// dry-run, so a first run reports what it would delete and deletes nothing.
const (
	registryPruneOff    = "off"
	registryPruneDryRun = "dry-run"
	registryPruneApply  = "apply"
	// registryPruneDefault is the mode of a run that names none.
	registryPruneDefault = registryPruneDryRun
	// registryKeepDefault is how many of the newest per-commit tags a retention
	// leaves when the operator names no count.
	registryKeepDefault = "5"
)

// registryPruneBudget bounds the whole retention, every registry request and
// command included, because the retention runs while this run holds the plugin
// lock and every push waits on that lock.
const registryPruneBudget = 10 * time.Minute

// registryContainer is the container the sandbox registry runs in; garbage
// collect runs inside it after a delete.
const registryContainer = "gt-registry"

// registryPushPattern matches a running push: pushes go through crane from a
// docker save tarball (plugins/polecat-image/plugin.md), so that is what
// garbage collect must not run beside.
const registryPushPattern = `^([^ ]*/)?crane push `

// registryLedgerMax bounds the maintenance ledger the plugin appends to, so a
// town's record rotates instead of growing without end.
const registryLedgerMax = 1 << 20

// PruneRegistry applies the registry retention behind POLECAT_IMAGE_REGISTRY_PRUNE
// (off, dry-run or apply; dry-run by default), after the other trims of a
// finished run.
//
// It runs only while this run holds the plugin lock, the lock every push holds,
// so a delete never races a push. Every read it cannot make refuses the plan
// rather than guessing, and no outcome fails the run: a retention that cannot
// run is logged and left to the next one.
func (r *Run) PruneRegistry() {
	if r.Cfg.Registry == "" {
		r.Logf("registry: retention skipped, no registry is configured")
		return
	}
	mode := r.Cfg.RegistryPrune
	if mode == "" {
		mode = registryPruneDefault
	}
	switch mode {
	case registryPruneOff:
		r.Logf("registry: retention is off (POLECAT_IMAGE_REGISTRY_PRUNE=off)")
		return
	case registryPruneDryRun, registryPruneApply:
	default:
		r.Logf("registry: retention skipped, POLECAT_IMAGE_REGISTRY_PRUNE=%s is not %s, %s or %s",
			mode, registryPruneOff, registryPruneDryRun, registryPruneApply)
		return
	}
	if !r.HaveLock {
		r.Logf("registry: retention skipped, this run does not hold the plugin lock")
		return
	}
	keepText := r.Cfg.RegistryKeep
	if keepText == "" {
		keepText = registryKeepDefault
	}
	keep, ok := KeepCount(keepText)
	if !ok {
		// A typo here must not read as "keep nothing": the images it would drop
		// are the ones a sandbox of an old commit still runs (gt-tpg0p.7).
		r.Logf("registry: nothing pruned: POLECAT_IMAGE_REGISTRY_KEEP=%s is not a whole number", keepText)
		return
	}

	ctx, cancel := r.pruneContext()
	defer cancel()
	target := r.registryTarget(ctx, keep)
	plan, err := target.Plan(ctx, diskmaint.Policy{Keep: keep, Now: r.clock()})
	if err != nil {
		r.Logf("registry: cannot plan retention: %v", err)
		return
	}
	if plan.Refused != "" {
		r.Logf("registry: retention refused: %s", plan.Refused)
		return
	}
	verb := "would delete"
	if mode == registryPruneApply {
		verb = "delete"
	}
	for _, action := range plan.Actions {
		r.Logf("registry: %s %s (%s, %s)", verb, action.Ref, action.Why, Human(action.Bytes))
	}
	if mode != registryPruneApply {
		r.Logf("registry: dry run, %d manifest(s) would be deleted and %d tag(s) kept; POLECAT_IMAGE_REGISTRY_PRUNE=apply applies the plan",
			len(plan.Actions), len(plan.Kept))
		return
	}

	// The target records every apply in the ledger, and the ledger's directory
	// is the one thing it cannot make for itself.
	ledger := r.Cfg.RegistryLedger
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		r.Logf("registry: not applied, cannot make the ledger directory %s: %v", filepath.Dir(ledger), err)
		return
	}
	result, err := target.Apply(ctx, plan)
	r.logRegistryApply(plan, result, err, ledger)
}

// pruneContext starts the retention's budget: the context every command and
// request of the retention runs under.
func (r *Run) pruneContext() (context.Context, context.CancelFunc) {
	if r.budget != nil {
		return r.budget()
	}
	return context.WithTimeout(context.Background(), registryPruneBudget)
}

// registryTarget is the registry retention target this run prunes through.
//
// The run holds the plugin lock, so the target applies directly instead of
// taking it. PushInFlight is still asked before garbage collect, so a push that
// got past the lock would still stop the collect. DockerHost is left empty:
// garbage collect inherits this process's DOCKER_HOST, as the pushes do.
func (r *Run) registryTarget(ctx context.Context, keep int) *registry.Target {
	return registry.NewTarget(registry.Config{
		Base:         "http://" + r.Cfg.Registry,
		Kubeconfig:   r.Cfg.RegistryKubeconfig,
		RecordsDir:   r.Cfg.SandboxRecords,
		Container:    registryContainer,
		Keep:         keep,
		Installed:    func() (string, error) { return r.registryInstalledCommit(ctx) },
		PushInFlight: func() bool { return r.registryPushInFlight(ctx) },
		Run:          r.registryRunner(),
		HTTP:         r.webClient(),
		Ledger:       diskmaint.NewLedger(r.Cfg.RegistryLedger, registryLedgerMax),
	})
}

// registryPushInFlight reports whether a crane push is running on this host,
// under the retention's budget. pgrep exits 1 when nothing matches; 0 is a
// match, and any other status is a probe that did not answer. The last two both
// report true, so garbage collect refuses rather than reading an unanswered
// probe as "no push".
func (r *Run) registryPushInFlight(ctx context.Context) bool {
	match := r.runner()(command{name: r.Cfg.Pgrep, args: []string{"-f", registryPushPattern}, ctx: ctx})
	return match.exit != 1
}

// registryInstalledCommit is the commit the installed gt reports, read like
// InstalledCommit but within the retention's budget. A gt version that exits
// nonzero is an error, which the planner refuses on, so no tag is deleted on a
// guess.
func (r *Run) registryInstalledCommit(ctx context.Context) (string, error) {
	res := r.runner()(command{name: r.Cfg.GT, args: []string{"version"}, ctx: ctx})
	if res.failed() {
		return "", fmt.Errorf("%s version exited %d", r.Cfg.GT, res.exit)
	}
	return commitOfVersion(res.output()), nil
}

// registryRunner answers the retention's command runs (kubectl for the in-use
// read, docker for garbage collect) through this run's runner, so a test answers
// them the way it answers every other program. The context carries the
// retention's budget into each command, so a command that hangs is ended with
// the budget.
//
// docker is the exception once it has started. Garbage collect runs inside the
// registry container, and killing the docker client leaves that collect running
// after the plugin lock is released, where it can race a push. So docker is not
// cancelled: the budget gates whether it starts, and the run waits for it.
//
// An environment entry with a value is carried to the command. An entry with an
// empty value sets nothing, so it is dropped and the command inherits this
// process's value for that variable.
func (r *Run) registryRunner() diskmaint.Runner {
	return func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run := ctx
		if name == "docker" {
			run = context.WithoutCancel(ctx)
		}
		res := r.runner()(command{name: name, args: args, env: setEntries(env), ctx: run})
		if res.failed() {
			// The error names the command and its status and never its output,
			// which can hold a secret.
			return nil, fmt.Errorf("%s exited %d", name, res.exit)
		}
		return []byte(res.output()), nil
	}
}

// setEntries is the environment entries that carry a value.
func setEntries(env []string) []string {
	var out []string
	for _, entry := range env {
		if _, value, _ := strings.Cut(entry, "="); value != "" {
			out = append(out, entry)
		}
	}
	return out
}

// logRegistryApply writes what an apply did (each digest it kept, with the
// registry's reason) and the ledger line that records the run.
func (r *Run) logRegistryApply(plan diskmaint.Plan, result diskmaint.Result, err error, ledger string) {
	for _, failure := range result.Failed {
		r.Logf("registry: kept %s (%s)", failure.Action.Ref, failure.Err)
	}
	if err != nil {
		r.Logf("registry: apply: %v", err)
		return
	}
	r.Logf("registry: deleted %d of %d manifest(s) and freed %s; ledger %s",
		len(result.Done), len(plan.Actions), Human(result.BytesFreed), ledger)
}
