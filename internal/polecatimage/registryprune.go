package polecatimage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// registryContainer is the container the sandbox registry runs in: garbage
// collect after a delete runs inside it, and a name failing validation runs
// nothing (docs/plans/2026-10-28-disk-maintenance-design.md).
const registryContainer = "gt-registry"

// registryLedgerMax bounds the maintenance ledger the plugin appends to, so a
// town's record rotates instead of growing without end.
const registryLedgerMax = 1 << 20

// PruneRegistry applies the registry retention behind POLECAT_IMAGE_REGISTRY_PRUNE
// — a dry run by default — after the other trims of a finished run.
//
// It runs only while this run holds the plugin lock, the lock every push holds,
// so a delete never races a push. Every read it cannot make refuses the plan
// rather than guessing, and no outcome fails the run: a retention that cannot
// run is logged and left to the next one (docs/plans/2026-10-28-disk-maintenance-design.md).
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

	target, ledger := r.registryTarget(keep)
	ctx := context.Background()
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
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		r.Logf("registry: not applied, cannot make the ledger directory %s: %v", filepath.Dir(ledger), err)
		return
	}
	result, err := target.Apply(ctx, plan)
	r.logRegistryApply(plan, result, err, ledger)
}

// registryTarget is the registry retention target this run prunes through, and
// the ledger its applies are recorded in.
//
// The run holds the plugin lock, so the target applies directly instead of
// taking it, and no push can be in flight: every push goes through this plugin
// under that lock. Garbage collect runs this run's own docker, the daemon the
// pushes went to.
func (r *Run) registryTarget(keep int) (*registry.Target, string) {
	runtime := filepath.Dir(r.Cfg.LockDir)
	ledger := filepath.Join(runtime, "diskmaint", "ledger.jsonl")
	return registry.NewTarget(registry.Config{
		Base:         "http://" + r.Cfg.Registry,
		Kubeconfig:   filepath.Join(runtime, "sbx-k8s", "kubeconfig"),
		RecordsDir:   r.Cfg.SandboxRecords,
		Container:    registryContainer,
		Keep:         keep,
		Installed:    r.installedCommit,
		PushInFlight: func() bool { return false },
		Run:          r.registryRunner(),
		HTTP:         r.webClient(),
		Ledger:       diskmaint.NewLedger(ledger, registryLedgerMax),
	}), ledger
}

// installedCommit is the commit the installed gt reports: gt version is the one
// source for the tag a retention keeps, and the target refuses a plan that
// cannot read it rather than deleting that tag
// (docs/plans/2026-10-28-disk-maintenance-design.md).
func (r *Run) installedCommit() (string, error) {
	return r.InstalledCommit(), nil
}

// registryRunner answers the retention's command runs — kubectl for the in-use
// read, docker for garbage collect — through this run's runner, so a test
// answers them the way it answers every other program.
//
// This run's runner carries no environment, so an entry with a value is refused
// rather than dropped: the command would otherwise run against a daemon other
// than the one the caller named. An entry with an empty value sets nothing and
// is dropped.
func (r *Run) registryRunner() diskmaint.Runner {
	return func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, entry := range env {
			if key, value, _ := strings.Cut(entry, "="); value != "" {
				return nil, fmt.Errorf("%s needs environment variable %s, which this run's runner cannot carry", name, key)
			}
		}
		res := r.call(name, args...)
		if res.failed() {
			return nil, fmt.Errorf("%s exited %d: %s", name, res.exit, lastLine(res.stderr))
		}
		return []byte(res.output()), nil
	}
}

// logRegistryApply writes what an apply did — each digest it kept, with the
// registry's reason — and the ledger line that records the run.
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
