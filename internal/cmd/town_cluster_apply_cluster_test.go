package cmd

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// clusterApplyFixture is a plan directory with one ConfigMap file, a plan.json
// naming cluster with id sha, and the approval record for that id.
func clusterApplyFixture(t *testing.T, cluster, sha string) (dir, approvalPath string) {
	t.Helper()
	dir = t.TempDir()
	data := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: gt\n")
	writeReadFile(t, filepath.Join(dir, "01-configmap-cm.yaml"), data)
	record := planRecord{Cluster: cluster, ID: sha, Files: []planRecordFile{{Name: "01-configmap-cm.yaml", Phase: planPhaseWorkloads, SHA256: clusterApplyFileSHA(data)}}}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshalling plan.json: %v", err)
	}
	writeReadFile(t, filepath.Join(dir, planFileName), body)
	approvalPath = filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, shapeApprovalFor(sha))
	return dir, approvalPath
}

var clusterApplyKubeconfigs = planFlags{applyKubeconfig: "/apply"}

// TestClusterShapeApplyGateRefusesAPlanForAnotherCluster: a flag that is not a
// cluster, a plan for another cluster and a plan naming none are refused before the
// verifier and any cluster call.
func TestClusterShapeApplyGateRefusesAPlanForAnotherCluster(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("b", 64)
	for _, tc := range []struct{ name, flag, plan, want string }{
		{"a flag that is not a cluster", "gt-prod", clusterControl, `--cluster "gt-prod" is not a cluster the apply knows`},
		{"a control plan under the gt-sbx flag", clusterSbx, clusterControl, "the plan is for cluster control, not --cluster gt-sbx"},
		{"a gt-sbx plan under the control flag", clusterControl, clusterSbx, "the plan is for cluster gt-sbx, not --cluster control"},
		{"a plan that names no cluster", clusterSbx, "", "the plan names no cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, approvalPath := clusterApplyFixture(t, tc.plan, sha)
			var verified, calls int
			err := runTownClusterApply(context.Background(), shapeKube(&calls), io.Discard, dir, approvalPath, clusterApplyKubeconfigs, applyClusterGate(tc.flag, shapeVerifier(&verified, nil)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterApply = %v, want a refusal naming %q", err, tc.want)
			}
			if verified != 0 || calls != 0 {
				t.Errorf("the refused apply verified %d times and called %d times, want none", verified, calls)
			}
		})
	}
}

// TestClusterShapeApplyPassesThePlansClusterToTheVerifier: a gt-sbx plan under its
// flag reaches the verifier with its cluster, and the apply then runs.
func TestClusterShapeApplyPassesThePlansClusterToTheVerifier(t *testing.T) {
	t.Parallel()
	dir, approvalPath := clusterApplyFixture(t, clusterSbx, strings.Repeat("b", 64))
	var seen []string
	var calls int
	kube := update.RunFunc(func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, int, error) {
		calls++
		return "", 0, nil
	})
	err := runTownClusterApply(context.Background(), kube, io.Discard, dir, approvalPath, clusterApplyKubeconfigs, applyClusterGate(clusterSbx, func(_ clusterapproval.Record, plan planRecord) error {
		seen = append(seen, plan.Cluster)
		return nil
	}))
	if err != nil {
		t.Fatalf("runTownClusterApply: %v", err)
	}
	if len(seen) != 1 || seen[0] != clusterSbx {
		t.Errorf("the verifier was given clusters %v, want one call with %q", seen, clusterSbx)
	}
	if calls == 0 {
		t.Errorf("the apply made no cluster call after the verifier passed")
	}
}

// TestClusterShapeApplyRecordVerifierUsesThePlanCluster: the approval verifier
// checks the record against the plan's own cluster; a plan with no cluster reads as
// control.
func TestClusterShapeApplyRecordVerifierUsesThePlanCluster(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 64)
	at := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	record := func(cluster string) clusterapproval.Record {
		return clusterapproval.Record{Act: clusterapproval.ActApplyPlan, Cluster: cluster, PlanSHA256: sha, ProposalID: "0123456789ab", ApprovedAt: at, ExpiresAt: at.Add(time.Hour)}
	}
	for _, tc := range []struct {
		name string
		rec  clusterapproval.Record
		plan string
		bad  bool
	}{
		{"a gt-sbx record for a gt-sbx plan", record(clusterSbx), clusterSbx, false},
		{"a control record for a gt-sbx plan", record(clusterControl), clusterSbx, true},
		{"a gt-sbx record for a control plan", record(clusterSbx), clusterControl, true},
		{"a control record for a plan with no cluster", record(clusterControl), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verify := applyRecordVerifier(func() time.Time { return at.Add(30 * time.Minute) })
			if err := verify(tc.rec, planRecord{Cluster: tc.plan, ID: sha}); (err != nil) != tc.bad {
				t.Errorf("verify = %v, want an error: %t", err, tc.bad)
			}
		})
	}
}
