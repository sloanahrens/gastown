package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/update"
)

// shapeKube is a RunFunc that counts every cluster call, so a run that applies
// nothing can be shown to have made none.
func shapeKube(calls *int) update.RunFunc {
	return func(context.Context, string, []string, string, ...string) (string, int, error) {
		*calls++
		return "", 0, nil
	}
}

// TestClusterShapeApplyUnreadablePlan: a plan that cannot be read returns the
// reader's error and makes no cluster call.
func TestClusterShapeApplyUnreadablePlan(t *testing.T) {
	t.Parallel()
	var calls int
	err := runTownClusterApply(context.Background(), shapeKube(&calls),
		filepath.Join(t.TempDir(), "plan"), filepath.Join(t.TempDir(), "approval.json"), readKubeconfs, nil)
	if err == nil || !strings.Contains(err.Error(), "the plan record cannot be read") {
		t.Fatalf("runTownClusterApply returned %v, want the plan read error", err)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}

// TestClusterShapeApplyReadablePlan: a plan that reads is not applied: the run
// returns an error saying apply is not built and nothing was applied, and makes
// no cluster call.
func TestClusterShapeApplyReadablePlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
	var calls int
	err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, filepath.Join(dir, "approval.json"), readKubeconfs, nil)
	if err == nil || !strings.Contains(err.Error(), "not built") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want an error saying apply is not built and nothing was applied", err)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}
