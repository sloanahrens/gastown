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

// TestClusterShapeApplyReadablePlan: a plan and approval that read are not
// applied: the run returns an error saying apply is not built and nothing was
// applied, and makes no cluster call.
func TestClusterShapeApplyReadablePlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
	writeReadFile(t, filepath.Join(dir, "approval.json"), readApprovalOK)
	var calls int
	err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, filepath.Join(dir, "approval.json"), readKubeconfs, nil)
	if err == nil || !strings.Contains(err.Error(), "not built") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want an error saying apply is not built and nothing was applied", err)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}

// TestClusterShapeApplyUnreadableApproval: a plan that reads and an approval
// that does not returns the approval reader's error and makes no cluster call,
// so the run never falls through to the not-built error on an unread approval.
func TestClusterShapeApplyUnreadableApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		missing bool   // no approval file at the path
		data    []byte // the approval file's bytes when it is written
		wantErr string
	}{
		{name: "a missing approval record", missing: true, wantErr: "there is no approval record"},
		{name: "a malformed approval record", data: []byte("{}"), wantErr: "is not a valid approval record"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
			writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
			approvalPath := filepath.Join(dir, "approval.json")
			if !tc.missing {
				writeReadFile(t, approvalPath, tc.data)
			}
			var calls int
			err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, approvalPath, readKubeconfs, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runTownClusterApply returned %v, want an error naming %q", err, tc.wantErr)
			}
			if calls != 0 {
				t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
			}
		})
	}
}
