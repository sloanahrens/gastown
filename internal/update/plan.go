package update

import (
	"context"
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

// PlanOptions narrows the plan: Only keeps the named steps, Skip drops the
// named step and the steps that follow it, and Force plans an install even when
// nothing changed.
type PlanOptions struct {
	Skip  map[StepName]bool
	Only  map[StepName]bool
	Force bool
}

// BuildPlan decides which update steps have work and returns them in run order
// with a reason for each, alongside the build the run targets. It does no I/O:
// every fact comes from p.
func BuildPlan(ctx context.Context, p Probes, o PlanOptions) ([]StepRecord, Target, error) {
	var target Target

	installed, err := p.InstalledSHA(ctx)
	if err != nil {
		return nil, target, fmt.Errorf("InstalledSHA: %w", err)
	}
	mainSHA, err := p.MainSHA(ctx)
	if err != nil {
		return nil, target, fmt.Errorf("MainSHA: %w", err)
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

// planInstall decides whether the installed build needs replacing.
func planInstall(installed, mainSHA string, published, force bool) (bool, string) {
	switch {
	case installed != mainSHA:
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

// applySelection applies Only and Skip after the probe decisions. A step that
// follows a skipped step is skipped too, so the plan keeps drain, restart,
// verify and lift mirroring install and lift mirroring drain.
func applySelection(steps []StepRecord, o PlanOptions) {
	skip := make(map[StepName]bool, len(o.Skip))
	for name, on := range o.Skip {
		if on {
			skip[name] = true
		}
	}
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
		case len(o.Only) > 0 && !o.Only[steps[i].Name]:
			steps[i].WillRun = false
			steps[i].Reason = "not selected by --only"
		case skip[steps[i].Name]:
			steps[i].WillRun = false
			steps[i].Reason = "skipped by --skip"
		}
	}
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
