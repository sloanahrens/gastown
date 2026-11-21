package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// cacheTrimClaimJobWaitVerb is the wait a claim-Job run makes for its Job, at
// the fifteen-minute bound the claim route allows the walk of a large claim.
const cacheTrimClaimJobWaitVerb = "wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=900s"

// cacheTrimClaimJobRun calls the claim-Job run on the fixture's claim.
func cacheTrimClaimJobRun(f *cacheTrimFixture, claim cacheTrimClaim, apply bool) error {
	return f.d.cacheTrimClaimJobRun(context.Background(), "/kubeconfig", claim, "gastown-polecat:dev", cacheTrimNamespace, 1<<30, 0, apply)
}

// TestCacheTrimClaimJobDryRunAppliesNothing is the bead's gate: a dry run logs
// the dry_run line naming the Job, makes no cluster call, and returns nil.
func TestCacheTrimClaimJobDryRunAppliesNothing(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	if err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, false); err != nil {
		t.Fatalf("a dry run = %v, want nil", err)
	}
	if calls := f.verbs(); len(calls) != 0 {
		t.Errorf("a dry run made cluster calls %v; want none", calls)
	}
	want := "dry_run: " + cacheTrimJob + ": one Job, which plans its list in the claim"
	if logged := f.log.String(); !strings.Contains(logged, want) {
		t.Errorf("the dry run logged %q; want a line carrying %q", logged, want)
	}
}

// TestCacheTrimClaimJobRunAppliesTheClaimJob pins that an apply builds the
// claim-carried Job in the claim's placement, applies it alone, and waits the
// claim route's bound for it.
func TestCacheTrimClaimJobRunAppliesTheClaimJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	claim := cacheTrimClaim{
		ClaimName:    cacheTrimClaimName,
		NodeSelector: map[string]string{"gt.dev/pool": "sandbox"},
		Tolerations:  []cachevol.Toleration{{Key: "gt.dev/pool", Operator: "Exists"}},
	}
	if err := cacheTrimClaimJobRun(f, claim, true); err != nil {
		t.Fatalf("an apply = %v, want nil", err)
	}
	if applied := f.applied(t); len(applied) != 1 || applied[0] != "Job "+cacheTrimJob {
		t.Errorf("the run applied %v; want the one claim Job %s and no shard ConfigMap", applied, cacheTrimJob)
	}
	pod := f.appliedTrimJob(t).Spec.Template.Spec
	if pod.NodeSelector["gt.dev/pool"] != "sandbox" {
		t.Errorf("the applied Job's nodeSelector = %v, want the claim's placement", pod.NodeSelector)
	}
	if _, ok := f.indexOf(cacheTrimClaimJobWaitVerb); !ok {
		t.Errorf("the run did not wait %s for its Job; calls: %v", cacheTrimClaimJobWaitVerb, f.verbs())
	}
}

// TestCacheTrimClaimJobRunReturnsTheApplyError pins that an apply the cluster
// refuses is returned as the error, carrying the first stderr line alone: the
// lines after it can repeat the object.
func TestCacheTrimClaimJobRunReturnsTheApplyError(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimApplyVerb: {stderr: "Error from server: forbidden\nkind: Job, name: " + cacheTrimJob, code: 1},
	})
	err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
	if err == nil {
		t.Fatalf("an apply the cluster refused = nil, want the error")
	}
	if !strings.Contains(err.Error(), "Error from server: forbidden") {
		t.Errorf("the returned error %q does not carry the server's first stderr line", err)
	}
	if strings.Contains(err.Error(), "kind: Job, name:") {
		t.Errorf("the returned error %q carries stderr past its first line", err)
	}
}

// TestCacheTrimClaimJobRunReportsAnUnfinishedJob pins that a Job still running
// at the bound is an error naming the Job and the wait, not a nil return.
func TestCacheTrimClaimJobRunReportsAnUnfinishedJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
	})
	err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
	want := "the trim Job " + cacheTrimJob + " did not finish within 15m0s"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a Job still running at the bound = %v, want an error carrying %q", err, want)
	}
}

// TestCacheTrimClaimJobRunRefusesAnUnsetClock pins that the manifest's refusal
// is returned before any cluster call: the pod's plan judges file ages by the
// clock, so a Job built without one would remove files by an unknown age.
func TestCacheTrimClaimJobRunRefusesAnUnsetClock(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	f.d.diskMaint.now = func() time.Time { return time.Time{} }
	err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
	if err == nil {
		t.Fatalf("a run with no clock = nil, want the build's refusal")
	}
	if calls := f.verbs(); len(calls) != 0 {
		t.Errorf("a run with no clock made cluster calls %v; want none", calls)
	}
}
