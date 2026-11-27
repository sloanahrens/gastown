package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/clusterapproval"
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

// shapeVerifier is an approvalVerifier that counts its calls and returns want,
// so a run can be shown to have verified once, with a chosen result, or not at
// all.
func shapeVerifier(calls *int, want error) approvalVerifier {
	return func(clusterapproval.Record, planRecord) error {
		*calls++
		return want
	}
}

// shapeApprovalFor is an approval record approving the plan sha256 sha.
func shapeApprovalFor(sha string) []byte {
	return []byte(`{"act":"apply-plan","cluster":"control","plan_sha256":"` + sha + `","proposal_id":"0123456789ab","approved_at":"2026-11-26T08:00:00Z","expires_at":"2026-11-26T09:00:00Z"}`)
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

// TestClusterShapeApplyReadablePlan: a plan and approval that read, and an
// approval the verifier accepts, are not applied: the run returns an error
// saying apply is not built and nothing was applied, and makes no cluster call.
func TestClusterShapeApplyReadablePlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
	writeReadFile(t, filepath.Join(dir, "approval.json"), readApprovalOK)
	var calls, verified int
	err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, filepath.Join(dir, "approval.json"), readKubeconfs, shapeVerifier(&verified, nil))
	if err == nil || !strings.Contains(err.Error(), "not built") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want an error saying apply is not built and nothing was applied", err)
	}
	if verified != 1 {
		t.Errorf("runTownClusterApply called verify %d times, want once", verified)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}

// TestClusterShapeApplyOtherPlan: an approval that authorizes another plan's
// sha256 is refused before verify, so the verifier is never called and no
// cluster call is made. Mutation: remove the sha comparison in
// runTownClusterApply, and verify is called, so this test fails.
func TestClusterShapeApplyOtherPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, shapeApprovalFor(strings.Repeat("cd", 32)))
	var calls, verified int
	err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil))
	if err == nil || !strings.Contains(err.Error(), "another plan") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want an error saying the approval is for another plan and nothing was applied", err)
	}
	if verified != 0 {
		t.Errorf("runTownClusterApply called verify %d times, want none", verified)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}

// TestClusterShapeApplyVerifierError: a verifier that refuses the approval ends
// the run with its error, saying nothing was applied, and no cluster call.
// Mutation: swallow the verifier's error, and the not-built error is returned
// instead, so this test fails.
func TestClusterShapeApplyVerifierError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, readApprovalOK)
	var calls, verified int
	err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, approvalPath, readKubeconfs, shapeVerifier(&verified, errors.New("the approval expired")))
	if err == nil || !strings.Contains(err.Error(), "the approval expired") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want the verifier's error saying nothing was applied", err)
	}
	if verified != 1 {
		t.Errorf("runTownClusterApply called verify %d times, want once", verified)
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
