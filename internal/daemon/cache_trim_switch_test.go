package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

// viaJobWithin runs cacheTrimClaimViaJob on the fixture's claim and returns its
// result, or an error if the call has not returned within bound: a Job that
// never finishes must end the call, not hang the test.
func viaJobWithin(bound time.Duration, f *cacheTrimFixture, apply bool, trimmed map[string]string) (bool, <-chan bool, error) {
	done := make(chan bool, 1)
	go func() {
		done <- f.d.cacheTrimClaimViaJob(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
			"gastown-polecat:dev", cacheTrimNamespace, 1<<30, apply, trimmed)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	select {
	case ok := <-done:
		return ok, done, nil
	case <-ctx.Done():
		return false, done, fmt.Errorf("cacheTrimClaimViaJob did not return within %s: a Job the fake never finishes hung the call", bound)
	}
}

// viaJob is viaJobWithin at the hang guard, failing the test on a hang.
func viaJob(t *testing.T, f *cacheTrimFixture, apply bool, trimmed map[string]string) bool {
	t.Helper()
	ok, _, err := viaJobWithin(workspaceHangGuard, f, apply, trimmed)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestCacheTrimViaJobHeldWaitFailsTheHelper pins that a claim Job wait the fake
// never finishes ends the helper with its named error at the bound it is given,
// so the guard the other tests rely on is driven.
func TestCacheTrimViaJobHeldWaitFailsTheHelper(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	// The abandoned call finishes once the hold is released, and it writes the run
	// into the town's temp dir: wait for it before the dir is removed. Registered
	// before the hold, so it runs after the hold's release (cleanups run last in,
	// first out) and before the temp dir goes.
	var finished <-chan bool
	t.Cleanup(func() {
		if finished != nil {
			<-finished
		}
	})
	held := cacheTrimFailWaitHold(t, f, cacheTrimClaimJobWaitVerb)
	var err error
	if _, finished, err = viaJobWithin(50*time.Millisecond, f, true, map[string]string{}); err == nil || !strings.Contains(err.Error(), "did not return within 50ms") {
		t.Fatalf("a held wait = %v; want the helper's named error at its bound", err)
	}
	held.await(t)
}

// TestCacheTrimViaJobUsesTheClaimJobOnly pins that the claim is trimmed by the
// claim Job and nothing else: the Job is applied, no exec runs in a seat pod,
// no lister Job is applied, and the run reports a trim.
func TestCacheTrimViaJobUsesTheClaimJobOnly(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 4 absent 1 kept 2\n"},
	})
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a finished claim Job = false, want true; log: %s", f.log)
	}
	if applied := f.applied(t); len(applied) != 1 || applied[0] != "Job "+cacheTrimJob {
		t.Errorf("the run applied %v; want only the claim Job %s", applied, cacheTrimJob)
	}
	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "exec ") || strings.Contains(verb, " exec ") {
			t.Errorf("the run made an exec call %q; the claim Job is the only route", verb)
		}
	}
}

// TestCacheTrimViaJobFailureReturnsFalse pins that a claim Job whose result
// line cannot be read is logged as a failure and returns false, never a
// recorded trim.
func TestCacheTrimViaJobFailureReturnsFalse(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: ""},
	})
	if viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a claim Job with no result line = true, want false")
	}
	if want := "cache trim " + cacheTrimClaimName + " in " + cacheTrimNamespace + ": failed: "; !strings.Contains(f.log.String(), want) {
		t.Errorf("the log lacks %q; log: %s", want, f.log)
	}
}

// TestCacheTrimViaJobRefusesASecondClaimInANamespace pins that a namespace
// whose claim was already trimmed this pass is a verdict: nothing is applied and
// the run returns true, as the old route did.
func TestCacheTrimViaJobRefusesASecondClaimInANamespace(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	if !viaJob(t, f, true, map[string]string{cacheTrimNamespace: "gt-cache-other"}) {
		t.Fatalf("a second claim in one namespace = false, want true (a verdict, not a retry)")
	}
	if applied := f.applied(t); len(applied) != 0 {
		t.Errorf("the refused claim applied %v; want nothing", applied)
	}
	if !strings.Contains(f.log.String(), "gt-cache-other") {
		t.Errorf("the refusal does not name the claim already trimmed; log: %s", f.log)
	}
}

// TestCacheTrimViaJobStopsAtAPodReadItCannotMake pins that a pod list the
// cluster cannot answer trims nothing and returns false.
func TestCacheTrimViaJobStopsAtAPodReadItCannotMake(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		"get pods -n " + cacheTrimNamespace + " -o json": {stderr: "error: connection refused", code: 1},
	})
	if viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("an unreadable pod list = true, want false")
	}
	if applied := f.applied(t); len(applied) != 0 {
		t.Errorf("the run applied %v with no sandbox count; want nothing", applied)
	}
}

// TestCacheTrimViaJobDryRunAppliesNothing pins that a dry run logs the Job it
// would apply, applies nothing and still returns true.
func TestCacheTrimViaJobDryRunAppliesNothing(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	if !viaJob(t, f, false, map[string]string{}) {
		t.Fatalf("a dry run = false, want true; log: %s", f.log)
	}
	if applied := f.applied(t); len(applied) != 0 {
		t.Errorf("a dry run applied %v; want nothing", applied)
	}
	if !strings.Contains(f.log.String(), "dry_run") {
		t.Errorf("a dry run logged no dry_run line; log: %s", f.log)
	}
}

// TestCacheTrimViaJobPassesTheSandboxCount pins that the Job's planner is told
// how many sandboxes mount the claim: the fixture's one running pod is `1`.
func TestCacheTrimViaJobPassesTheSandboxCount(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 0 absent 0 kept 0\n"},
	})
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a finished claim Job = false, want true; log: %s", f.log)
	}
	// The applied document is indented JSON, so the flag and its value are
	// matched through whitespace.
	for _, call := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(call.args), "apply ") && sandboxesFlag.MatchString(call.stdin) {
			return
		}
	}
	t.Error("no applied document carries --sandboxes 1; the planner was not told the sandbox count")
}

// sandboxesFlag is the claim Job's `--sandboxes 1` argument pair in an applied
// JSON document, whatever its indentation.
var sandboxesFlag = regexp.MustCompile(`"--sandboxes",\s*"1"`)

// sandboxesZeroFlag is the same pair for a claim no pod mounts.
var sandboxesZeroFlag = regexp.MustCompile(`"--sandboxes",\s*"0"`)

// TestCacheTrimViaJobAppliesWithZeroSandboxes pins that a readable pod list with
// no pod mounting the claim is not a refusal: the claim Job mounts the claim
// itself, so it is applied with a sandbox count of 0.
func TestCacheTrimViaJobAppliesWithZeroSandboxes(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		"get pods -n " + cacheTrimNamespace + " -o json": {stdout: `{"items":[]}`},
		cacheTrimClaimJobLogVerb:                         {stdout: "removed 0 absent 0 kept 0\n"},
	})
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a claim no pod mounts = false, want true; log: %s", f.log)
	}
	for _, call := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(call.args), "apply ") && sandboxesZeroFlag.MatchString(call.stdin) {
			return
		}
	}
	t.Error("no applied document carries --sandboxes 0 for a claim no pod mounts")
}

// TestCacheTrimSwitchRecordsTheRun pins that a finished claim trim appends one
// run with outcome ok and the Job's counts.
func TestCacheTrimSwitchRecordsTheRun(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 4 absent 1 kept 2\n"},
	})
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a finished claim Job = false, want true; log: %s", f.log.String())
	}
	runs := f.jobRuns(t)
	if len(runs) != 1 || runs[0].Outcome != cacheTrimRunOK {
		t.Fatalf("the ledger holds %+v, want one run with outcome %q", runs, cacheTrimRunOK)
	}
	for stat, n := range map[string]int64{cacheTrimStatTrimmedFiles: 4, cacheTrimStatAbsentFiles: 1, cacheTrimStatKeptFiles: 2} {
		if got := runs[0].Stats[stat]; got != n {
			t.Errorf("the run's %s = %d, want %d", stat, got, n)
		}
	}
}

// TestCacheTrimSwitchRecordsNoRunOnADryRunAndAFailedRunOnAnUnreadableResult pins that a dry run records no
// run, and that a Job whose result cannot be read records a failed run and never
// an ok run with zero counts.
func TestCacheTrimSwitchRecordsNoRunOnADryRunAndAFailedRunOnAnUnreadableResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		apply     bool
		wantOK    bool
		wantRuns  []string
		overrides map[string]cliReply
	}{
		{name: "dry run", apply: false, wantOK: true},
		{name: "unreadable result", apply: true, wantOK: false, wantRuns: []string{cacheTrimRunFailed}, overrides: map[string]cliReply{cacheTrimClaimJobLogVerb: {stdout: ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", tc.overrides)
			if got := viaJob(t, f, tc.apply, map[string]string{}); got != tc.wantOK {
				t.Errorf("viaJob = %v, want %v; log: %s", got, tc.wantOK, f.log.String())
			}
			var outcomes []string
			for _, run := range f.jobRuns(t) {
				outcomes = append(outcomes, run.Outcome)
			}
			if !slices.Equal(outcomes, tc.wantRuns) {
				t.Errorf("the ledger holds outcomes %v, want %v", outcomes, tc.wantRuns)
			}
		})
	}
}

// TestCacheTrimSwitchLogsTheFinish pins that a finished claim trim logs one line
// with the Job's counts and the time since its start, and a dry run logs none.
func TestCacheTrimSwitchLogsTheFinish(t *testing.T) {
	t.Parallel()
	for _, apply := range []bool{true, false} {
		f := newCacheTrimFixture(t, "", map[string]cliReply{
			cacheTrimClaimJobLogVerb: {stdout: "removed 4 absent 1 kept 2\n"},
		})
		// The first reading of the clock is the run's start; every later one is
		// ninety seconds on.
		var read atomic.Bool
		f.d.diskMaint.now = func() time.Time {
			if read.Swap(true) {
				return diskMaintNow.Add(90 * time.Second)
			}
			return diskMaintNow
		}
		if !viaJob(t, f, apply, map[string]string{}) {
			t.Fatalf("apply=%v: a claim Job = false, want true; log: %s", apply, f.log.String())
		}
		want := "cache trim " + cacheTrimClaimName + " in " + cacheTrimNamespace + " finished: removed 4 absent 1 kept 2 elapsed 1m30s"
		if got := strings.Contains(f.log.String(), want); got != apply {
			t.Errorf("apply=%v: the log holds the finish line = %v, want %v; log: %s", apply, got, apply, f.log.String())
		}
	}
}

// TestCacheTrimSwitchParksOnEveryFailure pins that a claim whose trim fails is
// parked: a failed apply, a failed Job and an unreadable result line each record
// one failed run, raise the claim's failure item and return false, and the next
// pass for that claim makes no cluster call, says PARKED and returns true.
func TestCacheTrimSwitchParksOnEveryFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		overrides map[string]cliReply
		hold      bool
	}{
		{"a failed apply", map[string]cliReply{cacheTrimApplyVerb: {stderr: "invalid manifest", code: 1}}, false},
		{"a failed Job", map[string]cliReply{
			cacheTrimFailWaitFailedVerb:  {},
			cacheTrimFailWaitTrimLogVerb: {stdout: "could not remove /claim/x: permission denied\n"},
		}, true},
		{"an unreadable result line", map[string]cliReply{cacheTrimClaimJobLogVerb: {stdout: ""}}, false},
		{"a pod list that cannot be read", map[string]cliReply{"get pods -n " + cacheTrimNamespace + " -o json": {stderr: "error: connection refused", code: 1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", tc.overrides)
			if tc.hold {
				cacheTrimFailWaitHold(t, f, cacheTrimFailWaitCompleteVerb)
			}
			if viaJob(t, f, true, map[string]string{}) {
				t.Fatalf("a failed trim = true, want false; log: %s", f.log.String())
			}
			if runs := f.jobRuns(t); len(runs) != 1 || runs[0].Outcome != cacheTrimRunFailed {
				t.Errorf("the ledger holds %+v, want one run with outcome %q", runs, cacheTrimRunFailed)
			}
			if !f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
				t.Errorf("the claim is not parked after its trim failed")
			}
			if items := f.noOpItems(t); len(items) != 1 || items[0].Key != cacheTrimFailedKey(cacheTrimNamespace, cacheTrimClaimName) {
				t.Errorf("the attention items = %+v, want the claim's one failure item", items)
			}
			calls := len(f.cli.recorded())
			if !viaJob(t, f, true, map[string]string{}) {
				t.Errorf("a parked claim = false, want true (a verdict, no retry)")
			}
			if got := len(f.cli.recorded()); got != calls {
				t.Errorf("the parked claim made %d more cluster calls, want none", got-calls)
			}
			if !strings.Contains(f.log.String(), "PARKED after a failure; nothing trimmed until the daemon restarts") {
				t.Errorf("the parked pass did not say PARKED; log: %s", f.log.String())
			}
		})
	}
}

// TestCacheTrimSwitchDoesNotParkAPendingJob pins that a Job still running when
// the wait ends is not a failure: nothing is parked, no run is recorded and the
// claim is a verdict, so the pass returns true.
func TestCacheTrimSwitchDoesNotParkAPendingJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
		cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
	})
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a pending Job = false, want true (a verdict); log: %s", f.log.String())
	}
	if f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Errorf("a Job still running parked its claim")
	}
	if runs := f.jobRuns(t); len(runs) != 0 {
		t.Errorf("a pending Job recorded %+v, want no run", runs)
	}
}

// TestCacheTrimPendingJobStartsNothingAndDoesNotPark pins that a claim Job still
// running at the wait is a verdict: logged as still running, no run recorded,
// not parked, and the pass returns true. A second pass while it runs applies
// no second Job.
func TestCacheTrimPendingJobStartsNothingAndDoesNotPark(t *testing.T) {
	t.Parallel()
	replies := map[string]cliReply{
		cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
		cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
	}
	f := newCacheTrimFixture(t, "", replies)
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a pending Job = false, want true (a verdict); log: %s", f.log.String())
	}
	want := "cache trim " + cacheTrimClaimName + " in " + cacheTrimNamespace + ": the trim Job " + cacheTrimJob + " is still running; its result is read at the next pass"
	if !strings.Contains(f.log.String(), want) || strings.Contains(f.log.String(), "failed:") {
		t.Errorf("the log lacks the still-running line or logs a failure; log: %s", f.log.String())
	}
	if runs := f.jobRuns(t); len(runs) != 0 || f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Errorf("a pending Job recorded %+v or parked the claim; want neither", runs)
	}
	// The Job the first pass applied is running now: the next pass must read it
	// as running and apply nothing.
	replies[cacheTrimExistingVerb] = cliReply{stdout: cacheTrimRunningJobJSON}
	replies[cacheTrimJobPodsVerb(cacheTrimJob)] = cacheTrimPodsReply(cacheTrimJobPodJSON("Running", diskMaintNow, "", cacheTrimForgejoPolecat))
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a second pass while the Job runs = false, want true (a verdict); log: %s", f.log.String())
	}
	if applied := f.applied(t); len(applied) != 1 || applied[0] != "Job "+cacheTrimJob {
		t.Errorf("the applied Jobs = %v; want only the first pass's %s, no second Job", applied, cacheTrimJob)
	}
}

// TestCacheTrimSwitchParkedClaimStillHoldsItsNamespace pins that a parked claim
// keeps the same-namespace marker: the failed Job and pods of the parked claim
// may still be there and a trim's labels cannot tell two claims' objects apart,
// so a second claim in that namespace is refused on the pass after the failure
// and applies nothing.
func TestCacheTrimSwitchParkedClaimStillHoldsItsNamespace(t *testing.T) {
	t.Parallel()
	const second = "gt-other-cache"
	f := newCacheTrimFixture(t, "", nil)
	f.d.cacheTrimPark(cacheTrimNamespace, cacheTrimClaimName, errors.New("an earlier pass failed"))
	trimmed := map[string]string{} // one pass's ledger, as cacheTrim builds it
	run := func(claim string) bool {
		return f.d.cacheTrimClaimViaJob(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: claim},
			"gastown-polecat:dev", cacheTrimNamespace, 1<<30, true, trimmed)
	}
	if !run(cacheTrimClaimName) {
		t.Fatalf("a parked claim = false, want true (a verdict)")
	}
	if !run(second) {
		t.Fatalf("a second claim in the parked claim's namespace = false, want true (refused)")
	}
	if applied := f.applied(t); len(applied) != 0 {
		t.Errorf("the second claim applied %v, want nothing: the parked claim still holds the namespace", applied)
	}
	if !strings.Contains(f.log.String(), "is trimmed in the same namespace") {
		t.Errorf("the second claim's refusal is not logged; log: %s", f.log.String())
	}
}

// cacheTrimCompletedJSON is the earlier trim Job as a finished one: its Complete
// condition is true and it is not active.
const cacheTrimCompletedJSON = `{"items":[{"kind":"Job","metadata":{"name":"` + cacheTrimJob + `","namespace":"` + cacheTrimNamespace + `",` +
	`"labels":{"app.kubernetes.io/name":"polecat-cache-trim","app.kubernetes.io/managed-by":"gt"}},` +
	`"status":{"conditions":[{"type":"Complete","status":"True"}]}}]}`

// TestCacheTrimSwitchParkIsPerClaim pins that a parked claim does not stop
// another claim's trim: claim A in openshell fails its apply and is parked, and
// claim B in another namespace, on the same pass, is trimmed and finished. The
// park is keyed on the namespace and the claim, so B is judged on its own.
func TestCacheTrimSwitchParkIsPerClaim(t *testing.T) {
	t.Parallel()
	const otherNamespace = "openshell-other"
	const otherClaim = "gt-other-cache"
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimApplyVerb:                           {stderr: "invalid manifest", code: 1},
		"get pods -n " + otherNamespace + " -o json": {stdout: `{"items":[]}`},
	})
	trimmed := map[string]string{} // one pass's ledger, shared by both claims
	if viaJob(t, f, true, trimmed) {
		t.Fatalf("claim A's failed trim = true, want false; log: %s", f.log.String())
	}
	if !f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Fatalf("claim A is not parked after its apply failed")
	}
	if !f.d.cacheTrimClaimViaJob(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: otherClaim},
		"gastown-polecat:dev", otherNamespace, 1<<30, true, trimmed) {
		t.Fatalf("claim B = false, want true (trimmed); log: %s", f.log.String())
	}
	if f.d.cacheTrimParked(otherNamespace, otherClaim) {
		t.Errorf("claim B is parked: claim A's park stopped its trim")
	}
	if applied := f.applied(t); !slices.Contains(applied, "Job "+cacheTrimJobName(otherClaim)) {
		t.Errorf("the applied documents = %v, want claim B's trim Job", applied)
	}
	if !strings.Contains(f.log.String(), "cache trim "+otherClaim+" in "+otherNamespace+" finished:") {
		t.Errorf("claim B's trim did not finish; log: %s", f.log.String())
	}
	if !f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Errorf("claim A lost its park after claim B's trim")
	}
}

// TestCacheTrimLateResultIsRead pins that an earlier claim Job which completed
// after its wait has its result line read and recorded at the next pass, before
// cacheTrimApply deletes it as stale and applies the new Job.
func TestCacheTrimLateResultIsRead(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimExistingVerb:    {stdout: cacheTrimCompletedJSON},
		cacheTrimClaimJobLogVerb: {stdout: "removed 3 absent 1 kept 2\n"},
	})
	f.d.cacheTrimSetPending(cacheTrimNamespace, cacheTrimClaimName, true)
	if !viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a pass beside a completed earlier Job = false, want true; log: %s", f.log.String())
	}
	read, ok := f.indexOf(cacheTrimClaimJobLogVerb)
	if !ok {
		t.Fatalf("the earlier Job's result line was never read; calls: %v", f.verbs())
	}
	deleteVerb := "delete job " + cacheTrimJob + " -n " + cacheTrimNamespace + " --ignore-not-found --wait=true --timeout=60s"
	deleted, ok := f.indexAfter(deleteVerb, read)
	if !ok {
		t.Fatalf("the stale Job was not deleted after its result was read; calls: %v", f.verbs())
	}
	if _, ok := f.indexAfter(cacheTrimApplyVerb, deleted); !ok {
		t.Errorf("the new Job was not applied after the stale one was deleted; calls: %v", f.verbs())
	}
	runs := f.jobRuns(t)
	if len(runs) == 0 || runs[0].Outcome != cacheTrimRunOK || runs[0].Stats[cacheTrimStatTrimmedFiles] != 3 {
		t.Errorf("the ledger holds %+v, want the earlier Job's run first, with outcome %q and 3 trimmed files", runs, cacheTrimRunOK)
	}
}

// TestCacheTrimLateResultIsNotReadUnlessCompleted pins the two refusals: an
// earlier Job still running and one that has not completed leave their result
// unread, so no run is recorded from a log that is not there yet.
func TestCacheTrimLateResultIsNotReadUnlessCompleted(t *testing.T) {
	t.Parallel()
	const head = `{"items":[{"kind":"Job","metadata":{"name":"` + cacheTrimJob + `","namespace":"` + cacheTrimNamespace + `",` +
		`"labels":{"app.kubernetes.io/name":"polecat-cache-trim","app.kubernetes.io/managed-by":"gt"}},"status":`
	for name, status := range map[string]string{
		"a Job still running":          `{"active":1}`,
		"a Job that has not completed": `{"conditions":[{"type":"Complete","status":"False"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", map[string]cliReply{
				cacheTrimExistingVerb: {stdout: head + status + `}]}`},
			})
			// A Job still running is the pending verdict (no failure, nothing recorded
			// from it); one not complete has nothing to read and applies the new Job.
			got := viaJob(t, f, true, map[string]string{})
			if name == "a Job still running" && !got {
				t.Errorf("%s: the pass verdict = false, want true (pending is a verdict)", name)
			}
			for _, run := range f.jobRuns(t) {
				if run.Outcome == cacheTrimRunFailed {
					t.Errorf("%s: a failed run was recorded: %+v", name, run)
				}
			}
			// The new Job's own result line is read after its apply; the earlier
			// Job's would be read before it. No read before the apply is the
			// refusal.
			read, readOK := f.indexOf(cacheTrimClaimJobLogVerb)
			apply, applyOK := f.indexOf(cacheTrimApplyVerb)
			if readOK && (!applyOK || read < apply) {
				t.Errorf("%s: the earlier Job's result line was read before the new Job was applied; calls: %v", name, f.verbs())
			}
		})
	}
}

// TestCacheTrimLateResultMalformedParks pins the alarming branch of the late
// read: an earlier Job that completed with no result line, or one that cannot be
// parsed, records one FAILED run and parks the claim, and the next pass for the
// claim makes no cluster call.
func TestCacheTrimLateResultMalformedParks(t *testing.T) {
	t.Parallel()
	for name, log := range map[string]string{
		"a missing result line":   "",
		"a malformed result line": "removed twelve absent 1 kept 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", map[string]cliReply{
				cacheTrimExistingVerb:    {stdout: cacheTrimCompletedJSON},
				cacheTrimClaimJobLogVerb: {stdout: log},
			})
			f.d.cacheTrimSetPending(cacheTrimNamespace, cacheTrimClaimName, true)
			if viaJob(t, f, true, map[string]string{}) {
				t.Fatalf("%s: an earlier Job read late = true, want false (a failure)", name)
			}
			if runs := f.jobRuns(t); len(runs) != 1 || runs[0].Outcome != cacheTrimRunFailed {
				t.Errorf("%s: the ledger holds %+v, want one run with outcome %q", name, runs, cacheTrimRunFailed)
			}
			if !f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
				t.Errorf("%s: the claim is not parked", name)
			}
			// A parked claim never reaches the late read again, and every other park
			// clears the mark, so a failed late read does too.
			if f.d.cacheTrimIsPending(cacheTrimNamespace, cacheTrimClaimName) {
				t.Errorf("%s: the failed late read left the pending mark set", name)
			}
			calls := len(f.cli.recorded())
			if !viaJob(t, f, true, map[string]string{}) {
				t.Errorf("%s: a parked claim = false, want true (a verdict)", name)
			}
			if got := len(f.cli.recorded()); got != calls {
				t.Errorf("%s: the parked claim made %d more cluster calls, want none", name, got-calls)
			}
		})
	}
}

// TestCacheTrimSwitchRaisesWhenTheDiskStaysFull pins the replacement for the
// over-the-cap alert the claim Job route cannot raise (it measures nothing): after
// a finished trim, a sandbox disk at or over the early trim percent raises one
// low item for the claim; one under it raises none; an unreadable disk raises
// none and says so once an hour; a dry run reads no disk.
func TestCacheTrimSwitchRaisesWhenTheDiskStaysFull(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		apply     bool
		reading   []disks.Reading
		wantItem  bool
		wantReads int
		wantLog   string
	}{
		{"at the percent", true, []disks.Reading{{Name: diskMaintDiskColima, UsedPct: 75}}, true, 1, ""},
		{"under the percent", true, []disks.Reading{{Name: diskMaintDiskColima, UsedPct: 74}}, false, 1, ""},
		{"an unreadable disk", true, []disks.Reading{{Name: diskMaintDiskColima, Err: errors.New("df failed")}}, false, 1, "the sandbox disk could not be read after the trim"},
		{"a dry run", false, []disks.Reading{{Name: diskMaintDiskColima, UsedPct: 99}}, false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", nil)
			reads := 0
			f.d.diskMaint.readDisks = func(context.Context) []disks.Reading { reads++; return tc.reading }
			if !viaJob(t, f, tc.apply, map[string]string{}) {
				t.Fatalf("%s: the pass = false, want true; log: %s", tc.name, f.log.String())
			}
			if reads != tc.wantReads {
				t.Errorf("%s: the disk was read %d times, want %d", tc.name, reads, tc.wantReads)
			}
			var raised int
			for _, it := range f.noOpItems(t) {
				if it.Key == cacheTrimNoOpKey(cacheTrimNamespace, cacheTrimClaimName) {
					raised++
					if it.Severity != attention.SeverityLow || !strings.Contains(it.Summary, "75%") {
						t.Errorf("%s: the item is %+v, want a low item naming the 75%% early percent", tc.name, it)
					}
				}
			}
			if (raised == 1) != tc.wantItem || raised > 1 {
				t.Errorf("%s: %d item(s) for the claim, want an item = %v", tc.name, raised, tc.wantItem)
			}
			if tc.wantLog != "" && !strings.Contains(f.log.String(), tc.wantLog) {
				t.Errorf("%s: the log lacks %q: %s", tc.name, tc.wantLog, f.log.String())
			}
		})
	}
}

// TestCacheTrimSwitchDiskItem pins the claim's no-op item by the disk a finished
// trim reads, not by the start of a pass: a finished trim on a full disk raises
// it and the next pass, whose Job is still running, leaves it raised; a finished
// trim under the percent clears it; an unreadable disk leaves it as it was; and
// a trim that completed after its wait decides it in that pass.
func TestCacheTrimSwitchDiskItem(t *testing.T) {
	t.Parallel()
	key := cacheTrimNoOpKey(cacheTrimNamespace, cacheTrimClaimName)
	claimItems := func(t *testing.T, f *cacheTrimFixture) int {
		t.Helper()
		var n int
		for _, it := range f.noOpItems(t) {
			if it.Key == key {
				n++
			}
		}
		return n
	}
	raise := func(f *cacheTrimFixture) {
		f.d.cacheTrimNoOp.raise(attention.Item{Key: key})
	}
	reading := func(pct float64) []disks.Reading {
		return []disks.Reading{{Name: diskMaintDiskColima, UsedPct: pct}}
	}
	// pending answers the claim Job's wait as one that ran out with the Job still
	// active, the state a pass that leaves the item undecided carries.
	pending := func(f *cacheTrimFixture) {
		f.answerFrom(cacheTrimClaimJobWaitVerb, cliReply{stderr: "error: timed out waiting for the condition\n", code: 1})
		f.answerFrom(cacheTrimJobStateVerb, cliReply{stdout: `{"status":{"active":1}}`})
	}

	t.Run("a full disk keeps its item through a pass whose Job is pending", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, "", nil)
		reads := 0
		f.d.diskMaint.readDisks = func(context.Context) []disks.Reading { reads++; return reading(99) }
		if !viaJob(t, f, true, map[string]string{}) {
			t.Fatalf("a finished trim on a full disk = false, want true; log: %s", f.log.String())
		}
		if got := claimItems(t, f); got != 1 {
			t.Fatalf("a finished trim on a disk at 99%% left %d item(s), want the one it raises", got)
		}
		pending(f)
		if !viaJob(t, f, true, map[string]string{}) {
			t.Fatalf("a pass whose Job is pending = false, want true (a verdict); log: %s", f.log.String())
		}
		if got := claimItems(t, f); got != 1 {
			t.Errorf("a pass whose Job is pending on the still-full disk left %d item(s), want the item the finished trim raised", got)
		}
		if reads != 1 {
			t.Errorf("the disk was read %d times, want the one read of the finished trim: a pending pass decides nothing", reads)
		}
	})

	t.Run("a finished trim under the percent clears the item", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, "", nil)
		f.d.diskMaint.readDisks = func(context.Context) []disks.Reading { return reading(50) }
		raise(f)
		if !viaJob(t, f, true, map[string]string{}) {
			t.Fatalf("a finished trim under the percent = false, want true; log: %s", f.log.String())
		}
		if got := claimItems(t, f); got != 0 {
			t.Errorf("a finished trim on a disk at 50%% left %d item(s), want none", got)
		}
	})

	for _, tc := range []struct {
		name   string
		raised bool
	}{
		{"an unreadable disk leaves the item raised", true},
		{"an unreadable disk raises nothing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", nil)
			f.d.diskMaint.readDisks = func(context.Context) []disks.Reading {
				return []disks.Reading{{Name: diskMaintDiskColima, Err: errors.New("df failed")}}
			}
			if tc.raised {
				raise(f)
			}
			if !viaJob(t, f, true, map[string]string{}) {
				t.Fatalf("%s: a finished trim = false, want true; log: %s", tc.name, f.log.String())
			}
			want := 0
			if tc.raised {
				want = 1
			}
			if got := claimItems(t, f); got != want {
				t.Errorf("%s: %d item(s) after the trim, want %d: an unreadable disk is neither full nor empty", tc.name, got, want)
			}
			if logged := f.log.String(); !strings.Contains(logged, "the attention item is left as it was") {
				t.Errorf("%s: the unreadable disk is not logged: %s", tc.name, logged)
			}
		})
	}

	t.Run("a late-completed trim under the percent clears a stale item", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, "", map[string]cliReply{
			cacheTrimExistingVerb:    {stdout: cacheTrimCompletedJSON},
			cacheTrimClaimJobLogVerb: {stdout: "removed 3 absent 1 kept 2\n"},
		})
		f.d.cacheTrimSetPending(cacheTrimNamespace, cacheTrimClaimName, true)
		// The new Job stays pending, so only the late trim's own decision can
		// clear the item in this pass.
		pending(f)
		f.d.diskMaint.readDisks = func(context.Context) []disks.Reading { return reading(50) }
		raise(f)
		if !viaJob(t, f, true, map[string]string{}) {
			t.Fatalf("a pass beside a late-completed trim = false, want true; log: %s", f.log.String())
		}
		if got := claimItems(t, f); got != 0 {
			t.Errorf("a trim that completed after its wait on a disk at 50%% left %d item(s), want the stale item cleared in that pass", got)
		}
	})
}

// TestCacheTrimDiskItemReadsAfterThePassBudgetEnds pins that a finished trim's
// disk read has its own budget: the item follows the disk after the pass ends.
func TestCacheTrimDiskItemReadsAfterThePassBudgetEnds(t *testing.T) {
	t.Parallel()
	key := cacheTrimNoOpKey(cacheTrimNamespace, cacheTrimClaimName)
	for _, tc := range []struct {
		name string
		used float64
		want int
	}{
		{"under the percent clears a raised item", 40, 0},
		{"at the percent raises the item", 80, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", nil)
			// A real read handed a context that ended reports no reading.
			f.d.diskMaint.readDisks = func(ctx context.Context) []disks.Reading {
				if err := ctx.Err(); err != nil {
					return []disks.Reading{{Name: diskMaintDiskColima, Err: err}}
				}
				return []disks.Reading{{Name: diskMaintDiskColima, UsedPct: tc.used}}
			}
			if tc.want == 0 {
				f.d.cacheTrimNoOp.raise(attention.Item{Key: key})
			}
			pass, endPass := context.WithCancel(context.Background())
			endPass()
			if !f.d.cacheTrimClaimViaJob(pass, "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
				"gastown-polecat:dev", cacheTrimNamespace, 1<<30, true, map[string]string{}) {
				t.Fatalf("%s: the pass = false, want true; log: %s", tc.name, f.log.String())
			}
			var got int
			for _, it := range f.noOpItems(t) {
				if it.Key == key {
					got++
				}
			}
			if got != tc.want {
				t.Errorf("%s: %d item(s) after the trim, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestCacheTrimLateResultDiskItemReadsAfterThePassBudgetEnds pins that the late
// result's disk read has its own budget too: a trim that completed after its
// wait still decides the item once the pass context has ended.
func TestCacheTrimLateResultDiskItemReadsAfterThePassBudgetEnds(t *testing.T) {
	t.Parallel()
	key := cacheTrimNoOpKey(cacheTrimNamespace, cacheTrimClaimName)
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimExistingVerb:    {stdout: cacheTrimCompletedJSON},
		cacheTrimClaimJobLogVerb: {stdout: "removed 3 absent 1 kept 2\n"},
		// The new Job stays pending, so only the late trim's own read decides the
		// item in this pass.
		cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition\n", code: 1},
		cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
	})
	// A real read handed a context that ended reports no reading.
	f.d.diskMaint.readDisks = func(ctx context.Context) []disks.Reading {
		if err := ctx.Err(); err != nil {
			return []disks.Reading{{Name: diskMaintDiskColima, Err: err}}
		}
		return []disks.Reading{{Name: diskMaintDiskColima, UsedPct: 50}}
	}
	f.d.cacheTrimSetPending(cacheTrimNamespace, cacheTrimClaimName, true)
	f.d.cacheTrimNoOp.raise(attention.Item{Key: key})
	pass, endPass := context.WithCancel(context.Background())
	endPass()
	if !f.d.cacheTrimClaimViaJob(pass, "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
		"gastown-polecat:dev", cacheTrimNamespace, 1<<30, true, map[string]string{}) {
		t.Fatalf("a pass beside a late-completed trim = false, want true; log: %s", f.log.String())
	}
	var got int
	for _, it := range f.noOpItems(t) {
		if it.Key == key {
			got++
		}
	}
	if got != 0 {
		t.Errorf("a late trim on a disk at 50%% left %d item(s), want the stale item cleared by the late result's own read", got)
	}
}

// TestCacheTrimSwitchPendingMark pins the pending mark the claim route keeps: a
// Job still running when its wait ends sets the claim's mark (and parks nothing),
// and a dry run neither sets a mark nor clears one the claim already carries.
func TestCacheTrimSwitchPendingMark(t *testing.T) {
	t.Parallel()
	// pendingOverrides answers the claim Job's wait as one that ran out with the
	// Job still active, the state a pass leaves pending.
	pendingOverrides := func() map[string]cliReply {
		return map[string]cliReply{
			cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition\n", code: 1},
			cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
		}
	}
	t.Run("a pending pass leaves the mark set", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, "", pendingOverrides())
		if !viaJob(t, f, true, map[string]string{}) {
			t.Fatalf("a pending Job = false, want true (a verdict); log: %s", f.log.String())
		}
		if !f.d.cacheTrimIsPending(cacheTrimNamespace, cacheTrimClaimName) {
			t.Errorf("a pending Job left no mark")
		}
		if f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
			t.Errorf("a pending Job parked its claim")
		}
	})
	t.Run("a dry run sets no mark", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, "", pendingOverrides())
		viaJob(t, f, false, map[string]string{})
		if f.d.cacheTrimIsPending(cacheTrimNamespace, cacheTrimClaimName) {
			t.Errorf("a dry run set a mark")
		}
	})
	t.Run("a dry run leaves a mark the claim already carries", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, "", pendingOverrides())
		viaJob(t, f, true, map[string]string{})
		viaJob(t, f, false, map[string]string{})
		if !f.d.cacheTrimIsPending(cacheTrimNamespace, cacheTrimClaimName) {
			t.Errorf("a dry run cleared a pending mark")
		}
	})
}

// TestCacheTrimSwitchPendingMarkClears pins that the mark a pending pass left
// clears when the claim's run ends: a finished apply, a failed apply and an
// unreadable pod list each clear it, and a dry run clears nothing.
func TestCacheTrimSwitchPendingMarkClears(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		apply     bool
		overrides map[string]cliReply
		wantMark  bool
	}{
		{"a finished pass clears the mark", true, nil, false},
		{"a failed apply clears the mark", true, map[string]cliReply{cacheTrimApplyVerb: {stderr: "invalid manifest", code: 1}}, false},
		{"an unreadable pod list clears the mark", true, map[string]cliReply{cacheTrimPodsVerb: {stderr: "error: connection refused", code: 1}}, false},
		{"a dry run keeps the mark", false, nil, true},
		{"a dry run with an unreadable pod list keeps the mark", false, map[string]cliReply{cacheTrimPodsVerb: {stderr: "error: connection refused", code: 1}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", tc.overrides)
			f.d.cacheTrimSetPending(cacheTrimNamespace, cacheTrimClaimName, true)
			viaJob(t, f, tc.apply, map[string]string{})
			if got := f.d.cacheTrimIsPending(cacheTrimNamespace, cacheTrimClaimName); got != tc.wantMark {
				t.Errorf("the mark after the pass = %v, want %v; log: %s", got, tc.wantMark, f.log.String())
			}
		})
	}
	t.Run("a dry run whose manifest is refused keeps the mark", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, "", nil)
		f.d.cacheTrimSetPending(cacheTrimNamespace, cacheTrimClaimName, true)
		// A dry run has no wait, so the direct call cannot hang; a negative cap is the
		// input the manifest builder refuses.
		if f.d.cacheTrimClaimViaJob(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
			"gastown-polecat:dev", cacheTrimNamespace, -1, false, map[string]string{}) {
			t.Errorf("a refused manifest = true, want false (a failure)")
		}
		if !f.d.cacheTrimIsPending(cacheTrimNamespace, cacheTrimClaimName) {
			t.Errorf("a dry run's failure cleared the mark")
		}
	})
}

// TestCacheTrimLateResultOnlyForAPendingClaim pins that the late read runs only
// for a claim a pass left pending. A finished run leaves its Job in the cluster
// until the next apply deletes it, so a claim with no mark must not record that
// Job a second time; a claim with the mark records the Job's run, then the pass's
// own, and the mark is spent. The late read runs before cacheTrimClaimJobRun, so
// it sees the mark before any clear ends it: a result is never lost to the order.
func TestCacheTrimLateResultOnlyForAPendingClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		mark     bool
		wantRuns int
	}{
		{"no mark records only the pass's own run", false, 1},
		{"the mark records the earlier Job's run and then the pass's own", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", map[string]cliReply{
				cacheTrimExistingVerb:    {stdout: cacheTrimCompletedJSON},
				cacheTrimClaimJobLogVerb: {stdout: "removed 3 absent 1 kept 2\n"},
			})
			f.d.cacheTrimSetPending(cacheTrimNamespace, cacheTrimClaimName, tc.mark)
			if !viaJob(t, f, true, map[string]string{}) {
				t.Fatalf("a pass beside a finished Job = false, want true; log: %s", f.log.String())
			}
			if runs := f.jobRuns(t); len(runs) != tc.wantRuns {
				t.Errorf("the ledger holds %d runs, want %d: %+v", len(runs), tc.wantRuns, runs)
			}
			if f.d.cacheTrimIsPending(cacheTrimNamespace, cacheTrimClaimName) {
				t.Errorf("the mark is still set after the pass")
			}
		})
	}
}
