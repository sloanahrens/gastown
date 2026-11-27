package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// rollbackSteps is the rollback's calls as steps: each undo, rollout wait and
// Available read in the order made. Any other call is listed too, so an apply shows.
func rollbackSteps(calls [][]string) []string {
	var steps []string
	for _, call := range calls {
		v := shapeVerb(call)
		switch {
		case len(v) > 2 && v[0] == "rollout" && v[1] == "undo":
			steps = append(steps, "undo "+v[2]+" "+v[len(v)-1])
		case len(v) > 2 && v[0] == "rollout" && v[1] == "status":
			steps = append(steps, "wait "+v[2])
		case len(v) > 2 && v[0] == "get" && strings.HasPrefix(v[len(v)-1], "jsonpath"):
			steps = append(steps, "ready "+v[2])
		default:
			steps = append(steps, "call "+strings.Join(v, " "))
		}
	}
	return steps
}

// rollbackLedgerLine reads the one-line rollback ledger in dir.
func rollbackLedgerLine(t *testing.T, dir string) applyLedgerEntry {
	t.Helper()
	ledger, err := os.ReadFile(filepath.Join(dir, rollbackLedgerName))
	if err != nil {
		t.Fatalf("reading the rollback ledger: %v", err)
	}
	var entry applyLedgerEntry
	if err := json.Unmarshal(ledger, &entry); err != nil {
		t.Fatalf("the rollback ledger %q is not one line: %v", ledger, err)
	}
	return entry
}

// TestTownClusterRollbackOrder: each Deployment is undone to its recorded revision in
// the reverse of the apply's order (trackerd, the dashboard, the daemon, the bd
// gateway), and each rollout and Available read finishes before the next undo.
func TestTownClusterRollbackOrder(t *testing.T) {
	t.Parallel()
	docs, files := shapeDaemonPlan()
	dir, _ := shapeWritePlan(t, docs, files)
	kube := &shapeLiveKube{answer: shapeAnswering(shapeDaemonHeld(), "True")}
	if err := runTownClusterRollback(context.Background(), kube.run, io.Discard, dir, "/keys/apply.kubeconfig"); err != nil {
		t.Fatalf("runTownClusterRollback = %v, want nil", err)
	}
	var want []string
	for _, d := range []struct{ name, revision string }{{k8sdaemon.TrackerName, "5"}, {k8sdaemon.DashboardName, "4"}, {k8sdaemon.Name, "3"}, {k8sdaemon.BdGatewayName, "6"}} {
		want = append(want, "undo deployment/"+d.name+" --to-revision="+d.revision, "wait deployment/"+d.name, "ready "+d.name)
	}
	if got := rollbackSteps(kube.calls); !slices.Equal(got, want) {
		t.Fatalf("the rollback's steps = %v, want %v", got, want)
	}
	if entry := rollbackLedgerLine(t, dir); entry.Result != rollbackResultUndone || len(entry.Objects) != 4 {
		t.Fatalf("the rollback ledger = %+v, want one undone line for four Deployments", entry)
	}
}

// TestTownClusterRollbackRefusesFlags: an empty plan, an empty kubeconfig and a
// flag-shaped kubeconfig each refuse before any read or kubectl call.
func TestTownClusterRollbackRefusesFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, plan, kubeconfig, want string }{
		{"empty plan", "", "/keys/apply.kubeconfig", "--plan and --kubeconfig must both name a file"},
		{"empty kubeconfig", t.TempDir(), "", "--plan and --kubeconfig must both name a file"},
		{"flag-shaped kubeconfig", t.TempDir(), "-x", "starts with '-'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kube := &shapeLiveKube{}
			err := runTownClusterRollback(context.Background(), kube.run, io.Discard, tc.plan, tc.kubeconfig)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterRollback = %v, want a refusal naming %q", err, tc.want)
			}
			if len(kube.calls) != 0 {
				t.Errorf("the rollback made %d calls, want none: %v", len(kube.calls), kube.calls)
			}
		})
	}
}

// TestTownClusterRollbackUndoFails: an undo kubectl refuses stops the rollback before
// the next Deployment, and the ledger records nothing undone.
func TestTownClusterRollbackUndoFails(t *testing.T) {
	t.Parallel()
	docs, files := shapeDaemonPlan()
	dir, _ := shapeWritePlan(t, docs, files)
	kube := &shapeLiveKube{exit: 1}
	err := runTownClusterRollback(context.Background(), kube.run, io.Discard, dir, "/keys/apply.kubeconfig")
	if err == nil || !strings.Contains(err.Error(), "the undo of deployment/"+k8sdaemon.TrackerName+" failed") {
		t.Fatalf("runTownClusterRollback = %v, want the trackerd undo's failure", err)
	}
	if got := rollbackSteps(kube.calls); len(got) != 1 {
		t.Errorf("the rollback's steps = %v, want only the trackerd undo", got)
	}
	if entry := rollbackLedgerLine(t, dir); entry.Result != rollbackResultRefused || len(entry.Objects) != 0 {
		t.Errorf("the rollback ledger = %+v, want one refused line with no objects", entry)
	}
}

// TestTownClusterRollbackWaitFails: a Deployment undone but not ready again stops the
// rollback, and the ledger records it as partial with that Deployment undone.
func TestTownClusterRollbackWaitFails(t *testing.T) {
	t.Parallel()
	docs, files := shapeDaemonPlan()
	dir, _ := shapeWritePlan(t, docs, files)
	kube := &shapeLiveKube{answer: shapeAnsweringNames(shapeDaemonHeld(), func(name string) string {
		if name == k8sdaemon.TrackerName {
			return "False"
		}
		return "True"
	})}
	err := runTownClusterRollback(context.Background(), kube.run, io.Discard, dir, "/keys/apply.kubeconfig")
	if err == nil || !strings.Contains(err.Error(), "deployment/"+k8sdaemon.TrackerName+" was undone but did not come back ready") {
		t.Fatalf("runTownClusterRollback = %v, want the trackerd readiness failure", err)
	}
	if entry := rollbackLedgerLine(t, dir); entry.Result != rollbackResultPartial || len(entry.Objects) != 1 {
		t.Errorf("the rollback ledger = %+v, want one partial line naming the undone trackerd", entry)
	}
}

// TestTownClusterRollbackMissingRevision: a Deployment with no recorded revision
// refuses before any kubectl call, since an undo to revision 0 means the previous one.
func TestTownClusterRollbackMissingRevision(t *testing.T) {
	t.Parallel()
	docs, files := shapeDaemonPlan()
	for i := range files {
		if strings.HasSuffix(files[i].Name, "-deployment-"+k8sdaemon.BdGatewayName+".yaml") {
			files[i].Revision = ""
		}
	}
	dir, _ := shapeWritePlan(t, docs, files)
	kube := &shapeLiveKube{answer: shapeAnswering(shapeDaemonHeld(), "True")}
	err := runTownClusterRollback(context.Background(), kube.run, io.Discard, dir, "/keys/apply.kubeconfig")
	if err == nil || !strings.Contains(err.Error(), "records no revision") || !strings.Contains(err.Error(), "nothing was rolled back") {
		t.Fatalf("runTownClusterRollback = %v, want a refusal naming the missing revision", err)
	}
	if len(kube.calls) != 0 {
		t.Errorf("the rollback made %d calls, want none: %v", len(kube.calls), kube.calls)
	}
}
