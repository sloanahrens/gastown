package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/clusterapproval"
)

var (
	applySHA      = strings.Repeat("ab", 32)
	applyOtherSHA = strings.Repeat("cd", 32)
	applyKubeconf = planFlags{policyKubeconfig: "/keys/policy.kubeconfig", applyKubeconfig: "/keys/apply.kubeconfig"}
	applyNow      = time.Date(2026, 11, 26, 8, 30, 0, 0, time.UTC)
)

// applyKube is the kubectl fake: it records every call, and answers a Deployment
// get from live (name to resourceVersion). A name missing from live is absent,
// and liveExit is the exit every get returns.
type applyKube struct {
	calls    [][]string
	live     map[string]string
	liveExit int
}

func (k *applyKube) run(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	k.calls = append(k.calls, args)
	if len(args) < 4 || args[2] != "get" {
		return "", 1, nil
	}
	if k.liveExit != 0 {
		return "", k.liveExit, nil
	}
	rv, ok := k.live[strings.TrimPrefix(args[3], "deployment/")]
	if !ok {
		return "", 0, nil
	}
	return `{"metadata":{"resourceVersion":"` + rv + `"}}`, 0, nil
}

// writes counts the recorded calls that are not a get.
func (k *applyKube) writes() int {
	n := 0
	for _, args := range k.calls {
		if len(args) < 3 || args[2] != "get" {
			n++
		}
	}
	return n
}

// applyVerifier is the approval fake: it counts its calls and answers err.
type applyVerifier struct {
	calls int
	err   error
}

func (v *applyVerifier) verify(clusterapproval.Record, clusterapproval.Plan, map[string]string, time.Time) error {
	v.calls++
	return v.err
}

// applyPlanDir writes a plan record that holds one Deployment, web in gastown, at
// resourceVersion 7 in phase W, and returns its directory.
func applyPlanDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	doc := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: gastown\n"
	writeApplyFile(t, filepath.Join(dir, "deployment-web.yaml"), []byte(doc))
	record, err := json.Marshal(planRecord{ID: applySHA, Files: []planRecordFile{{Name: "deployment-web.yaml", Phase: planPhaseWorkloads, SHA256: "00", Revision: "3", ResourceVersion: "7"}}})
	if err != nil {
		t.Fatal(err)
	}
	writeApplyFile(t, filepath.Join(dir, planFileName), record)
	return dir
}

// applyApproval is a control-cluster approval of the plan sha, valid across applyNow.
func applyApproval(sha string) []byte {
	return []byte(`{"act":"apply-plan","cluster":"control","plan_sha256":"` + sha + `","proposal_id":"0123456789ab","approved_at":"2026-11-26T08:00:00Z","expires_at":"2026-11-26T09:00:00Z"}`)
}

func writeApplyFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClusterShapeApplyRefusesWithNothingApplied: an apply with no approval record,
// or an approval that does not verify against the plan, refuses with zero apply
// calls. The approval is read before the cluster, so a missing or unparseable
// record makes no call at all.
func TestClusterShapeApplyRefusesWithNothingApplied(t *testing.T) {
	tests := []struct {
		name      string
		approval  []byte // nil: no record is written
		live      map[string]string
		liveExit  int
		verifyErr error
		wantErr   string
		wantCalls int
		wantCheck int
	}{
		{name: "no approval record", wantErr: "there is no approval record"},
		{name: "an approval that does not parse", approval: []byte("{}"), wantErr: "is not a valid approval record"},
		{name: "an approval that fails to verify", approval: applyApproval(applyOtherSHA), live: map[string]string{"web": "7"}, verifyErr: errors.New("the record approves the plan"), wantErr: "does not verify against the plan", wantCalls: 1, wantCheck: 1},
		{name: "an unreadable live Deployment", approval: applyApproval(applySHA), liveExit: 1, wantErr: "could not be read", wantCalls: 1},
		{name: "a live Deployment with no resourceVersion", approval: applyApproval(applySHA), live: map[string]string{"web": ""}, wantErr: "has no resourceVersion", wantCalls: 1},
		{name: "an approval that verifies is still refused", approval: applyApproval(applySHA), live: map[string]string{"web": "7"}, wantErr: "does not apply the plan yet", wantCalls: 1, wantCheck: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			approvalPath := filepath.Join(t.TempDir(), "approval.json")
			if tc.approval != nil {
				writeApplyFile(t, approvalPath, tc.approval)
			}
			kube := &applyKube{live: tc.live, liveExit: tc.liveExit}
			verifier := &applyVerifier{err: tc.verifyErr}
			err := runTownClusterApply(context.Background(), kube.run, applyPlanDir(t), approvalPath, applyKubeconf, verifier.verify, applyNow)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("apply returned %v, want a refusal naming %q", err, tc.wantErr)
			}
			if n := kube.writes(); n != 0 {
				t.Errorf("apply made %d non-get kubectl calls, want 0", n)
			}
			if len(kube.calls) != tc.wantCalls {
				t.Errorf("apply made %d kubectl calls, want %d", len(kube.calls), tc.wantCalls)
			}
			if verifier.calls != tc.wantCheck {
				t.Errorf("the approval was verified %d times, want %d", verifier.calls, tc.wantCheck)
			}
			for _, args := range kube.calls {
				if len(args) < 2 || args[0] != "--kubeconfig" || args[1] != applyKubeconf.applyKubeconfig {
					t.Errorf("kubectl ran %v, want only under the apply kubeconfig", args)
				}
			}
		})
	}
}

// TestClusterShapeApplyRefusesAnotherPlansApproval: the real verifier refuses an
// approval whose plan sha256 is another plan's, with zero apply calls.
func TestClusterShapeApplyRefusesAnotherPlansApproval(t *testing.T) {
	approvalPath := filepath.Join(t.TempDir(), "approval.json")
	writeApplyFile(t, approvalPath, applyApproval(applyOtherSHA))
	kube := &applyKube{live: map[string]string{"web": "7"}}
	err := runTownClusterApply(context.Background(), kube.run, applyPlanDir(t), approvalPath, applyKubeconf, clusterapproval.Verify, applyNow)
	if err == nil || !strings.Contains(err.Error(), "does not verify against the plan") {
		t.Fatalf("apply of another plan's approval returned %v, want a refusal that the approval does not verify", err)
	}
	if n := kube.writes(); n != 0 {
		t.Errorf("apply made %d non-get kubectl calls, want 0", n)
	}
}

// TestClusterShapeApplyRefusesBeforeAnyRead: a missing kubeconfig, one that reads
// as a flag, or no plan record refuses before any cluster call and before the
// approval is verified.
func TestClusterShapeApplyRefusesBeforeAnyRead(t *testing.T) {
	tests := []struct {
		name     string
		flags    planFlags
		noRecord bool
		wantErr  string
	}{
		{name: "the apply kubeconfig is missing", flags: planFlags{policyKubeconfig: "/keys/policy.kubeconfig"}, wantErr: "has files but no --apply-kubeconfig"},
		{name: "the apply kubeconfig reads as a flag", flags: planFlags{applyKubeconfig: "-x"}, wantErr: "starts with '-'"},
		{name: "there is no plan record", flags: applyKubeconf, noRecord: true, wantErr: "the plan record cannot be read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := applyPlanDir(t)
			if tc.noRecord {
				dir = t.TempDir()
			}
			approvalPath := filepath.Join(t.TempDir(), "approval.json")
			writeApplyFile(t, approvalPath, applyApproval(applySHA))
			kube := &applyKube{live: map[string]string{"web": "7"}}
			verifier := &applyVerifier{}
			err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, tc.flags, verifier.verify, applyNow)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("apply returned %v, want a refusal naming %q", err, tc.wantErr)
			}
			if len(kube.calls) != 0 || verifier.calls != 0 {
				t.Errorf("apply made %d kubectl calls and %d verifications, want none", len(kube.calls), verifier.calls)
			}
		})
	}
}
