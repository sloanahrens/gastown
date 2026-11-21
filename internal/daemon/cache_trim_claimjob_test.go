package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// cacheTrimClaimJobWaitVerb is the wait a claim-Job run makes for its Job, at
// the twenty-minute bound the claim route allows the walk of a large claim.
const cacheTrimClaimJobWaitVerb = "wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=1200s"

// cacheTrimClaimJobLogVerb is the read of the claim Job's trim container log,
// the one cacheTrimRemovedCounts parses.
const cacheTrimClaimJobLogVerb = "logs job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " -c trim"

// cacheTrimClaimJobCounts calls the claim-Job run on the fixture's claim.
func cacheTrimClaimJobCounts(f *cacheTrimFixture, claim cacheTrimClaim, apply bool) (removed, absent, kept int64, err error) {
	return f.d.cacheTrimClaimJobRun("/kubeconfig", claim, "gastown-polecat:dev", cacheTrimNamespace, 1<<30, 0, apply)
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

// TestCacheTrimClaimJobPendingIsNotFailure is the bead's first acceptance: a
// Job still running when the wait ends, with no Failed condition, is pending; a
// Failed Job is not, and nor is one found complete, read as a finished Job's.
func TestCacheTrimClaimJobPendingIsNotFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status string
		pending      bool
		wantErr      bool
		removed      int64
	}{
		{"still running", `{"status":{"active":1}}`, true, true, 0},
		{"a Failed condition", `{"status":{"conditions":[{"type":"Failed","status":"True"}]}}`, false, true, 0},
		{"complete after the wait", `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`, false, false, 4},
	} {
		f := newCacheTrimFixture(t, "", map[string]cliReply{
			cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
			cacheTrimJobStateVerb:     {stdout: tc.status},
			cacheTrimClaimJobLogVerb:  {stdout: "removed 4 absent 0 kept 1\n"},
		})
		removed, _, _, err := cacheTrimClaimJobCounts(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
		if got := errors.Is(err, errCacheTrimJobPending); got != tc.pending || (err != nil) != tc.wantErr || removed != tc.removed {
			t.Errorf("%s: the run = %d removed, %v (pending %v); want %d removed, pending %v, an error %v", tc.name, removed, err, got, tc.removed, tc.pending, tc.wantErr)
		}
		if tc.pending && (err == nil || !strings.Contains(err.Error(), "still running after the 20m0s wait")) {
			t.Errorf("%s: the pending error %v does not name the wait that ended", tc.name, err)
		}
	}
}

// TestCacheTrimClaimJobPendingNeedsAReadableActiveStatus is the bead's second
// acceptance: only a status read as active is pending. A failed read, garbled
// JSON and a status that is none of the three are ordinary, parked errors.
func TestCacheTrimClaimJobPendingNeedsAReadableActiveStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		reply   cliReply
		pending bool
	}{
		{"a failed status read", cliReply{stderr: "Error from server (NotFound): jobs.batch \"x\" not found\nthe rest", code: 1}, false},
		{"garbled JSON", cliReply{stdout: `{"status":`}, false},
		{"active 0 and no condition", cliReply{stdout: `{"status":{"active":0}}`}, false},
		{"active 1 and no condition", cliReply{stdout: `{"status":{"active":1}}`}, true},
	} {
		f := newCacheTrimFixture(t, "", map[string]cliReply{
			cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
			cacheTrimJobStateVerb:     tc.reply,
		})
		err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
		if got := errors.Is(err, errCacheTrimJobPending); err == nil || got != tc.pending {
			t.Fatalf("%s: the run = %v; errors.Is(err, errCacheTrimJobPending) = %v, want an error with %v", tc.name, err, got, tc.pending)
		}
		if !tc.pending && !strings.Contains(err.Error(), "reading the status of the trim Job "+cacheTrimJob) {
			t.Errorf("%s: the error %q does not name the status read of the Job", tc.name, err)
		}
		if strings.Contains(err.Error(), "the rest") {
			t.Errorf("%s: the error %q carries stderr past its first line", tc.name, err)
		}
	}
}

// TestCacheTrimClaimJobPendingEarlierJobRunning is the bead's acceptance: an
// earlier trim Job still running when this trim applies makes the run pending,
// not failed, and the refusal keeps the text that names the Job and why it is
// left alone.
func TestCacheTrimClaimJobPendingEarlierJobRunning(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimExistingVerb:              {stdout: cacheTrimRunningJobJSON},
		cacheTrimJobPodsVerb(cacheTrimJob): cacheTrimPodsReply(cacheTrimJobPodJSON("Running", diskMaintNow, "", cacheTrimForgejoPolecat)),
	})
	err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
	if !errors.Is(err, errCacheTrimJobPending) {
		t.Fatalf("a run beside an earlier Job still running = %v, want an error wrapping errCacheTrimJobPending", err)
	}
	want := "the earlier trim Job " + cacheTrimJob + " is still running; its pods mount the claim, so it is left to finish"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the error %q does not carry the refusal %q", err, want)
	}
}

// TestCacheTrimClaimJobNotPending pins that only the refusal for an earlier Job
// read as running is pending. A listing that cannot be read or decoded, the
// running Job's pod read that fails, and a stale object's delete that fails are
// ordinary errors: a later bead parks on pending, so an error wrapped by
// mistake would hide a failed trim as a healthy one.
func TestCacheTrimClaimJobNotPending(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		overrides map[string]cliReply
	}{
		{"the listing read fails", map[string]cliReply{
			cacheTrimExistingVerb: {stderr: "Error from server: forbidden", code: 1},
		}},
		{"the listing names no items list", map[string]cliReply{
			cacheTrimExistingVerb: {stdout: `{}`},
		}},
		{"the running Job's pod read fails", map[string]cliReply{
			cacheTrimExistingVerb:              {stdout: cacheTrimRunningJobJSON},
			cacheTrimJobPodsVerb(cacheTrimJob): {stderr: "Error from server: forbidden", code: 1},
		}},
		{"the stale object's delete fails", map[string]cliReply{
			cacheTrimExistingVerb: {stdout: cacheTrimExistingJSON},
			"delete job " + cacheTrimJob + " -n " + cacheTrimNamespace + " --ignore-not-found --wait=true --timeout=60s": {stderr: "Error from server: forbidden", code: 1},
		}},
	} {
		f := newCacheTrimFixture(t, "", tc.overrides)
		err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
		if err == nil || errors.Is(err, errCacheTrimJobPending) {
			t.Errorf("%s: the run = %v; want an error that is not errCacheTrimJobPending", tc.name, err)
		}
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

// onClaimJobCall runs fn before each cluster call whose argv carries verb, and
// then the call's own answer.
func (f *cacheTrimFixture) onClaimJobCall(verb string, fn func()) {
	run := f.d.execCmd
	f.d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(cmd.Args, " "), verb) {
			fn()
		}
		return run(cmd)
	}
}

// TestCacheTrimClaimJobBudgetNamesThePhaseThatWasCut pins that a claim run whose
// budget ends names the phase it ended in, the apply or the wait, and neither
// says the Job is still running.
func TestCacheTrimClaimJobBudgetNamesThePhaseThatWasCut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase, cutVerb string
		overrides      map[string]cliReply
	}{
		{"apply", cacheTrimApplyVerb, map[string]cliReply{
			cacheTrimApplyVerb: {stderr: "context canceled", code: 1},
		}},
		{"wait", cacheTrimClaimJobWaitVerb, map[string]cliReply{
			cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
			cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
		}},
	} {
		f := newCacheTrimFixture(t, "", tc.overrides)
		daemonCtx, stopDaemon := context.WithCancel(context.Background())
		f.d.ctx = daemonCtx
		f.onClaimJobCall(tc.cutVerb, stopDaemon)
		err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
		stopDaemon()
		want := "the trim Job " + cacheTrimJob + " was cut off during the " + tc.phase + " after "
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "still running") {
			t.Errorf("a budget cut during the %s = %v; want an error carrying %q and not saying the Job is still running", tc.phase, err, want)
		}
	}
}

// probeVerdict is the probe's decision for one call: a context that has ended is
// an error, except for the failed-condition wait, whose context ends when the
// complete leg wins, by design.
func probeVerdict(verb string, ctxErr error) error {
	if ctxErr == nil || strings.HasPrefix(verb, "wait --for=condition=failed ") {
		return nil
	}
	return fmt.Errorf("%q ran on a context already ended (%v)", verb, ctxErr)
}

// TestCacheTrimClaimJobBudgetOutlivesThePassContext pins that every cluster call
// of a claim run (the apply, the wait, the result read) is made on the claim's
// own budget: the run takes no caller context, so a caller-held context ends
// during the wait without reaching it, and each call's context is still live,
// carrying its own deadline of the wait plus the slack. The budget is read from
// the context the call reaches exec.CommandContext with (cacheTrimCtxSeen), the
// only place the kill a pass context causes can be seen.
func TestCacheTrimClaimJobBudgetOutlivesThePassContext(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 4 absent 1 kept 2\n"},
	})
	_, endPass := context.WithCancel(context.Background())
	defer endPass()
	// The wait legs run on two goroutines, so the count is atomic. The budget is
	// the twenty-minute wait plus the two-minute slack, written as literals: a
	// bound read from the constants would move with a changed slack and pass.
	const wantBudget = 20*time.Minute + 2*time.Minute
	var calls atomic.Int32
	f.d.cacheTrimCtxSeen = func(ctx context.Context, verb string) {
		n := calls.Add(1)
		if err := probeVerdict(verb, ctx.Err()); err != nil {
			t.Errorf("cluster call %d: %v; want the claim's own live budget", n, err)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Errorf("cluster call %d had no deadline; want the claim's own, the wait plus the slack", n)
			return
		}
		if left := time.Until(deadline); left < wantBudget-time.Minute || left > wantBudget {
			t.Errorf("cluster call %d had a deadline %v away; want the wait plus the slack, %v within a minute", n, left, wantBudget)
		}
	}
	f.onClaimJobCall(cacheTrimClaimJobWaitVerb, endPass)
	removed, absent, kept, err := f.d.cacheTrimClaimJobRun("/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName}, "gastown-polecat:dev", cacheTrimNamespace, 1<<30, 0, true)
	if err != nil || removed != 4 || absent != 1 || kept != 2 {
		t.Fatalf("a run whose pass context ends during the wait = %d, %d, %d, %v; want 4, 1, 2, nil", removed, absent, kept, err)
	}
	if n := calls.Load(); n < 3 {
		t.Errorf("the probe saw %d cluster calls; want the apply, the wait and the result read at least", n)
	}
}

// TestCacheTrimClaimJobBudgetNamesTheBoundThatCutIt is the bead's acceptance: a
// wait the claim Job's own context ends five minutes in, before the wait bound,
// is an error naming the elapsed time and that context, never the pending text.
// The status reads active on purpose: without the budget's own verdict the run
// would report the Job pending.
func TestCacheTrimClaimJobBudgetNamesTheBoundThatCutIt(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
		cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
	})
	clk := newFixedClock()
	f.d.clock = clk
	daemonCtx, endContext := context.WithCancel(context.Background())
	defer endContext()
	f.d.ctx = daemonCtx
	// The Job's budget is a child of d.ctx, so ending the daemon's context
	// during the wait ends the Job's own context; the fake clock dates the cut.
	f.onClaimJobCall(cacheTrimClaimJobWaitVerb, func() {
		clk.Advance(5 * time.Minute)
		endContext()
	})
	err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
	if err == nil {
		t.Fatalf("a wait its own context cut off = nil, want the cut-off error")
	}
	got := err.Error()
	if !strings.Contains(got, "cut off during the wait after 5m0s") || !strings.Contains(got, "by its context") {
		t.Errorf("the cut-off error %q does not name the elapsed time and the context that cut it", got)
	}
	for _, pending := range []string{"did not finish within", "still running after"} {
		if strings.Contains(got, pending) {
			t.Errorf("the cut-off error %q carries the pending text %q", got, pending)
		}
	}
}

// TestCacheTrimClaimJobBudgetNamesTheContextThatCutsTheWait pins that a wait the
// daemon's context cuts off is an error naming that context, not a Job still
// running, though the Job's status reads active.
func TestCacheTrimClaimJobBudgetNamesTheContextThatCutsTheWait(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
		cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
	})
	daemonCtx, stopDaemon := context.WithCancel(context.Background())
	defer stopDaemon()
	f.d.ctx = daemonCtx
	f.onClaimJobCall(cacheTrimClaimJobWaitVerb, stopDaemon)
	err := cacheTrimClaimJobRun(f, cacheTrimClaim{ClaimName: cacheTrimClaimName}, true)
	if err == nil || !strings.Contains(err.Error(), "the trim Job "+cacheTrimJob+" was cut off") {
		t.Fatalf("a wait its context cut off = %v; want an error naming the Job and the cut-off", err)
	}
}

// TestProbeVerdictFailsOnlyOnANonFailedWaitOnACancelledContext pins the probe's
// decision, so a typo in the verb it exempts cannot silently disable it: a
// context that has ended is an error for every call except the failed-condition
// wait, whose context ends when the complete leg wins, and a live one never is.
func TestProbeVerdictFailsOnlyOnANonFailedWaitOnACancelledContext(t *testing.T) {
	t.Parallel()
	const (
		complete = "wait --for=condition=complete job/trim -n ns --timeout=1200s"
		apply    = "apply -f -"
		failed   = "wait --for=condition=failed job/trim -n ns --timeout=1200s"
	)
	for _, row := range []struct {
		name      string
		verb      string
		cancelled bool
		wantErr   bool
	}{
		{"complete wait, cancelled", complete, true, true},
		{"apply, cancelled", apply, true, true},
		{"failed-condition wait, cancelled", failed, true, false},
		{"complete wait, live", complete, false, false},
		{"apply, live", apply, false, false},
		{"failed-condition wait, live", failed, false, false},
	} {
		var ctxErr error
		if row.cancelled {
			ctxErr = context.Canceled
		}
		if err := probeVerdict(row.verb, ctxErr); (err != nil) != row.wantErr {
			t.Errorf("%s: probeVerdict = %v, want error %v", row.name, err, row.wantErr)
		}
	}
}
