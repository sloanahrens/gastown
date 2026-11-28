package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/formula"
)

// syncDaemonFormulas makes the town's formulas match the binary and logs what
// moved: a pod roll runs no sync, so the town keeps the image's older text (gt-d3o38z).
// Orphaned and unowned files are named in the log, not removed; they are the
// operator's to promote into source or delete (gt-fd2cu.3).
func syncDaemonFormulas(update func(beadsPath string) (*formula.SyncPlan, error), townRoot string, logf func(format string, args ...any)) error {
	plan, err := update(townRoot)
	if err != nil {
		return fmt.Errorf("sync the town formulas under %s: %w", townRoot, err)
	}
	parts := []string{fmt.Sprintf("%d changed", plan.Changed())}
	for _, g := range []struct {
		label string
		names []string
	}{
		{"updated", plan.Updated()}, {"reinstalled", plan.Reinstalled()},
		{"installed", plan.Installed()}, {"drift replaced", plan.ReplacedDrift()},
	} {
		if len(g.names) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", g.label, strings.Join(g.names, ", ")))
		}
	}
	logf("formulas synced: %s", strings.Join(parts, "; "))
	if names := plan.Orphaned(); len(names) > 0 {
		logf("formulas orphaned: this binary no longer ships them and they stay on disk: %s", strings.Join(names, ", "))
	}
	if names := plan.Unowned(); len(names) > 0 {
		logf("formulas unowned: gt did not write them and this binary does not ship them: %s", strings.Join(names, ", "))
	}
	return nil
}
