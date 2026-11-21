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

// cacheTrimClaimJobLogVerb is the read of the claim Job's trim container log,
// the one cacheTrimRemovedCounts parses.
const cacheTrimClaimJobLogVerb = "logs job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " -c trim"

// cacheTrimClaimJobCounts calls the claim-Job run on the fixture's claim.
func cacheTrimClaimJobCounts(f *cacheTrimFixture, claim cacheTrimClaim, apply bool) (removed, absent, kept int64, err error) {
	return f.d.cacheTrimClaimJobRun(context.Background(), "/kubeconfig", claim, "gastown-polecat:dev", cacheTrimNamespace, 1<<30, 0, apply)
}

// cacheTrimClaimJobRun calls the claim-Job run on the fixture's claim and keeps
// only its error.
func cacheTrimClaimJobRun(f *cacheTrimFixture, claim cacheTrimClaim, apply bool) error {
	_, _, _, err := cacheTrimClaimJobCounts(f, claim, apply)
	return err
}

// TestCacheTrimClaimJobReadsTheResultLine pins that a finished Job's result
// line is the run's counts: the line the trim container prints last is what the
// run reports.
func TestCacheTrimClaimJobReadsTheResultLine(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 12 absent 1 kept 3\n"},
	})
	removed, absent, kept, err := cacheTrimClaimJobCounts(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
	if err != nil {
		t.Fatalf("a finished Job with a result line = %v, want nil", err)
	}
	if removed != 12 || absent != 1 || kept != 3 {
		t.Errorf("the run read %d removed, %d absent, %d kept; want 12, 1, 3", removed, absent, kept)
	}
}

// TestCacheTrimClaimJobFailsOnAMissingOrMalformedLine pins that a finished Job
// whose result line cannot be read fails the run: a line read as zero would
// record a trim that removed nothing.
func TestCacheTrimClaimJobFailsOnAMissingOrMalformedLine(t *testing.T) {
	t.Parallel()
	for _, log := range []string{"", "removed twelve absent 1 kept 3\n"} {
		f := newCacheTrimFixture(t, "", map[string]cliReply{
			cacheTrimClaimJobLogVerb: {stdout: log},
		})
		removed, absent, kept, err := cacheTrimClaimJobCounts(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
		if err == nil {
			t.Fatalf("a finished Job whose log is %q = %d, %d, %d, nil; want an error, never zero counts with nil", log, removed, absent, kept)
		}
	}
}

// TestCacheTrimClaimJobDryRunAppliesNothing is the bead's gate: a dry run logs
// the dry_run line naming the Job, makes no cluster call, and returns nil.
func TestCacheTrimClaimJobDryRunAppliesNothing(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	removed, absent, kept, err := cacheTrimClaimJobCounts(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, false)
	if err != nil {
		t.Fatalf("a dry run = %v, want nil", err)
	}
	if removed != 0 || absent != 0 || kept != 0 {
		t.Errorf("a dry run read %d removed, %d absent, %d kept; want zero counts", removed, absent, kept)
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

// TestCacheTrimClaimJobApplyErrorsAreReturned is the bead's acceptance: an apply
// the cluster refuses and a Job still running at the wait each come back as an
// error, the second naming the Job and the wait.
func TestCacheTrimClaimJobApplyErrorsAreReturned(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		overrides map[string]cliReply
		want      string
	}{
		{
			name:      "the cluster refuses the apply",
			overrides: map[string]cliReply{cacheTrimApplyVerb: {stderr: "Error from server: forbidden", code: 1}},
			want:      "Error from server: forbidden",
		},
		{
			name:      "the Job is still running at the wait",
			overrides: map[string]cliReply{cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1}},
			want:      "the trim Job " + cacheTrimJob + " did not finish within 15m0s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", tc.overrides)
			err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the run = %v, want an error carrying %q", err, tc.want)
			}
		})
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
