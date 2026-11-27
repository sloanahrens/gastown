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

// shapeLiveKube is a RunFunc that records every kubectl call's arguments and
// answers each read with out at exit, failing with err, so a test can choose the
// cluster's answer and show which calls the run made.
type shapeLiveKube struct {
	calls [][]string
	out   string
	exit  int
	err   error
}

func (k *shapeLiveKube) run(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	k.calls = append(k.calls, args)
	return k.out, k.exit, k.err
}

// shapeApplies counts recorded calls that carry an apply verb, so a run that
// applied nothing can be shown to have made no apply call.
func shapeApplies(calls [][]string) int {
	var n int
	for _, args := range calls {
		for _, arg := range args {
			if arg == "apply" {
				n++
				break
			}
		}
	}
	return n
}

// shapeLiveAt is the get answer for the plan's web Deployment at version.
func shapeLiveAt(version string) string {
	return `{"metadata":{"name":"web","namespace":"gastown","resourceVersion":"` + version + `"}}`
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

// TestClusterShapeApplyReadablePlan: a plan and approval that read, an approval
// the verifier accepts, and a live Deployment at the plan's resourceVersion are
// not applied: the run re-reads the Deployment under the plan's kubeconfig, then
// returns an error saying apply is not built and nothing was applied, with no
// apply call.
func TestClusterShapeApplyReadablePlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
	writeReadFile(t, filepath.Join(dir, "approval.json"), readApprovalOK)
	kube := &shapeLiveKube{out: shapeLiveAt("7")}
	var verified int
	err := runTownClusterApply(context.Background(), kube.run, dir, filepath.Join(dir, "approval.json"), readKubeconfs, shapeVerifier(&verified, nil))
	if err == nil || !strings.Contains(err.Error(), "not built") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want an error saying apply is not built and nothing was applied", err)
	}
	if verified != 1 {
		t.Errorf("runTownClusterApply called verify %d times, want once", verified)
	}
	if len(kube.calls) != 1 || shapeApplies(kube.calls) != 0 {
		t.Fatalf("runTownClusterApply made the calls %v, want one read and no apply", kube.calls)
	}
	if !strings.Contains(strings.Join(kube.calls[0], " "), "--kubeconfig "+readKubeconfs.applyKubeconfig) {
		t.Errorf("the re-read's arguments %v do not carry the plan's phase kubeconfig", kube.calls[0])
	}
}

// TestClusterShapeApplyClusterMoved: a held Deployment the cluster no longer
// holds at the plan's resourceVersion ends the run before any apply, saying
// nothing was applied: another version, no version, an object that is gone, and
// a read that failed are each refused. Mutation: remove the version comparison
// in recheckApplyDeployments, and each row reaches the not-built error, so this
// test fails.
func TestClusterShapeApplyClusterMoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		exit    int
		err     error
		wantErr string
	}{
		{name: "the live Deployment is at another resourceVersion", out: shapeLiveAt("8"), wantErr: "not the version the plan names"},
		{name: "the live Deployment has no resourceVersion", out: `{"metadata":{}}`, wantErr: "not the version the plan names"},
		{name: "the cluster no longer holds the Deployment", wantErr: "no longer holds"},
		{name: "the read exits nonzero", out: `{}`, exit: 1, wantErr: "cannot re-read"},
		{name: "the read fails", err: errors.New("kubectl not found"), wantErr: "cannot re-read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
			writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))
			approvalPath := filepath.Join(dir, "approval.json")
			writeReadFile(t, approvalPath, readApprovalOK)
			kube := &shapeLiveKube{out: tc.out, exit: tc.exit, err: tc.err}
			var verified int
			err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was applied") {
				t.Fatalf("runTownClusterApply returned %v, want the moved-cluster refusal naming %q and nothing applied", err, tc.wantErr)
			}
			if verified != 1 {
				t.Errorf("runTownClusterApply called verify %d times, want once", verified)
			}
			if len(kube.calls) != 1 || shapeApplies(kube.calls) != 0 {
				t.Errorf("runTownClusterApply made the calls %v, want one read and no apply", kube.calls)
			}
		})
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
