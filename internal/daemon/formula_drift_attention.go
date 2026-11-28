package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/formula"
)

// formulasStaleKey is the one item the collector raises (gt-u661sv).
const formulasStaleKey = "formulas-stale"

// collectFormulasStale raises one item while the town's formula copies differ
// from the binary's, naming every formula a sync writes; orphaned and unowned
// files alone raise nothing, and a failed read is returned (gt-u661sv).
func (s *attentionSources) collectFormulasStale(_ context.Context) ([]attention.Item, error) {
	read := s.formulasStalePlan
	if read == nil {
		// A town with no formulas directory has no copies to differ: the
		// container start wrote none (gt-qgh0t4). Any other stat failure falls
		// through to the plan read, whose error is returned below.
		if _, err := os.Stat(filepath.Join(s.d.config.TownRoot, ".beads", "formulas")); os.IsNotExist(err) {
			return nil, nil
		}
		read = formula.PlanFormulaSync
	}
	plan, err := read(s.d.config.TownRoot)
	if err != nil {
		return nil, fmt.Errorf("planning the formula sync of %s: %w", s.d.config.TownRoot, err)
	}
	if plan.Changed() == 0 {
		return nil, nil
	}
	return []attention.Item{{
		Key: formulasStaleKey, Kind: attention.KindFormulasStale,
		Severity: attention.SeverityHigh, Summary: formulasStaleSummary(plan),
	}}, nil
}

// formulasStaleSummary names every formula a sync would write.
func formulasStaleSummary(plan *formula.SyncPlan) string {
	var names []string
	for _, group := range [][]string{plan.Installed(), plan.Reinstalled(), plan.Updated(), plan.ReplacedDrift()} {
		names = append(names, group...)
	}
	return fmt.Sprintf("town formulas differ from the binary's: %s; gt formula sync writes them", strings.Join(names, ", "))
}
