package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Probes reports the town's state to the plan; a method that cannot answer
// returns an error rather than a default.
type Probes interface {
	InstalledSHA(ctx context.Context) (string, error)
	MainSHA(ctx context.Context) (string, error)
	ArchiveBehind(ctx context.Context) (commits, events int, err error)
	BackupToday(ctx context.Context) (bool, error)
	GCDue(ctx context.Context) (due bool, reason string, err error)
	ImagePublished(ctx context.Context, sha string) (bool, error)
}

// PlanOptions narrows the plan. Only, when it has at least one true-valued
// entry, keeps just the named steps. Skip drops each named step along with the
// steps that depend on it: skipping install also drops drain, restart, verify
// and lift, and skipping drain also drops lift. Force plans an install even
// when the installed build equals main and that image is published.
type PlanOptions struct {
	Skip  map[StepName]bool
	Only  map[StepName]bool
	Force bool
}

// BuildPlan decides which update steps have work and returns them in run order
// with a reason for each, alongside the build the run targets. It does no I/O:
// every fact comes from p. Both shas are trimmed of surrounding whitespace
// before they are compared, queried and recorded. A probe that fails, or that
// reports a blank sha, fails the plan with that probe named.
func BuildPlan(ctx context.Context, p Probes, o PlanOptions) ([]StepRecord, Target, error) {
	var target Target

	installed, err := p.InstalledSHA(ctx)
	if err != nil {
		return nil, target, fmt.Errorf("InstalledSHA: %w", err)
	}
	installed = strings.TrimSpace(installed)
	if installed == "" {
		return nil, target, errors.New("InstalledSHA: the probe returned an empty sha")
	}
	mainSHA, err := p.MainSHA(ctx)
	if err != nil {
		return nil, target, fmt.Errorf("MainSHA: %w", err)
	}
	mainSHA = strings.TrimSpace(mainSHA)
	if mainSHA == "" {
		return nil, target, errors.New("MainSHA: the probe returned an empty sha")
	}
	target = Target{SHA: mainSHA, InstalledSHA: installed}

	published, err := p.ImagePublished(ctx, mainSHA)
	if err != nil {
		return nil, target, fmt.Errorf("ImagePublished: %w", err)
	}
	commits, events, err := p.ArchiveBehind(ctx)
	if err != nil {
		return nil, target, fmt.Errorf("ArchiveBehind: %w", err)
	}
	backup, err := p.BackupToday(ctx)
	if err != nil {
		return nil, target, fmt.Errorf("BackupToday: %w", err)
	}
	gcDue, gcReason, err := p.GCDue(ctx)
	if err != nil {
		return nil, target, fmt.Errorf("GCDue: %w", err)
	}

	install, installReason := planInstall(installed, mainSHA, published, o.Force)
	maintenance, maintenanceReason := planMaintenance(commits, events, backup, gcDue, gcReason)

	followReason := "no install is planned"
	if install {
		followReason = "an install is planned"
	}

	steps := []StepRecord{
		{Name: StepDrain, WillRun: install, Reason: followReason},
		{Name: StepMaintenance, WillRun: maintenance, Reason: maintenanceReason},
		{Name: StepInstall, WillRun: install, Reason: installReason},
		{Name: StepRestart, WillRun: install, Reason: followReason},
		{Name: StepVerify, WillRun: install, Reason: followReason},
		{Name: StepLift, WillRun: install, Reason: followReason},
	}
	applySelection(steps, o)
	return steps, target, nil
}

// planInstall decides whether the installed build needs replacing. The
// installed build is current when its sha names the same commit as main, so a
// short installed sha matches main's full one; sameCommit holds that rule, and
// values under seven characters never match, so an unknown build is replaced.
func planInstall(installed, mainSHA string, published, force bool) (bool, string) {
	switch {
	case !sameCommit(installed, mainSHA):
		return true, fmt.Sprintf("installed build %s differs from main %s", shortSHA(installed), shortSHA(mainSHA))
	case !published:
		return true, fmt.Sprintf("the image for main %s is not published", shortSHA(mainSHA))
	case force:
		return true, "install forced"
	default:
		return false, "installed build equals main"
	}
}

// planMaintenance decides whether the archive, backup or gc need this run and
// names every trigger that fired.
func planMaintenance(commits, events int, backup, gcDue bool, gcReason string) (bool, string) {
	var reasons []string
	if commits > 0 || events > 0 {
		reasons = append(reasons, fmt.Sprintf("the archive is behind: %d commits and %d events", commits, events))
	}
	if !backup {
		reasons = append(reasons, "today's backup is missing")
	}
	if gcDue {
		reason := "a gc is due"
		if gcReason != "" {
			reason += ": " + gcReason
		}
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return false, "nothing is due"
	}
	return true, strings.Join(reasons, "; ")
}

// applySelection applies Only and Skip after the probe decisions. Both maps
// count only true-valued entries. Skip drops each step that depends on a
// skipped one: install brings drain, restart, verify and lift with it, and
// drain brings lift, so skipping install drops all five and skipping drain
// drops lift while install stays planned. A step turned off by a selection
// reason names the flag that turned it off.
func applySelection(steps []StepRecord, o PlanOptions) {
	skip := trueValued(o.Skip)
	only := trueValued(o.Only)

	if skip[StepInstall] {
		skip[StepDrain] = true
		skip[StepRestart] = true
		skip[StepVerify] = true
	}
	if skip[StepDrain] {
		skip[StepLift] = true
	}

	for i := range steps {
		switch {
		case len(only) > 0 && !only[steps[i].Name]:
			steps[i].WillRun = false
			steps[i].Reason = "not selected by --only"
		case skip[steps[i].Name]:
			steps[i].WillRun = false
			steps[i].Reason = "skipped by --skip"
		}
	}
}

// trueValued returns the entries of m that are set to true.
func trueValued(m map[StepName]bool) map[StepName]bool {
	set := make(map[StepName]bool, len(m))
	for name, on := range m {
		if on {
			set[name] = true
		}
	}
	return set
}

// shortSHA is the first eight characters of a sha, or the whole sha when it is
// shorter.
func shortSHA(sha string) string {
	const n = 8
	if len(sha) <= n {
		return sha
	}
	return sha[:n]
}
