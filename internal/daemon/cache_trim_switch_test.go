package daemon

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// viaJobWithin runs cacheTrimClaimViaJob on the fixture's claim and returns its
// result, or an error if the call has not returned within bound: a Job that
// never finishes must end the call, not hang the test.
func viaJobWithin(bound time.Duration, f *cacheTrimFixture, apply bool, trimmed map[string]string) (bool, error) {
	done := make(chan bool, 1)
	go func() {
		done <- f.d.cacheTrimClaimViaJob(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
			"gastown-polecat:dev", cacheTrimNamespace, 1<<30, apply, trimmed)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	select {
	case ok := <-done:
		return ok, nil
	case <-ctx.Done():
		return false, fmt.Errorf("cacheTrimClaimViaJob did not return within %s: a Job the fake never finishes hung the call", bound)
	}
}

// viaJob is viaJobWithin at the hang guard, failing the test on a hang.
func viaJob(t *testing.T, f *cacheTrimFixture, apply bool, trimmed map[string]string) bool {
	t.Helper()
	ok, err := viaJobWithin(workspaceHangGuard, f, apply, trimmed)
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
	held := cacheTrimFailWaitHold(t, f, cacheTrimClaimJobWaitVerb)
	if _, err := viaJobWithin(50*time.Millisecond, f, true, map[string]string{}); err == nil || !strings.Contains(err.Error(), "did not return within 50ms") {
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

// TestCacheTrimSwitchRecordsNothingUnlessTrimmed pins that a dry run records no
// run, and that a Job whose result cannot be read records a failed run and never
// an ok run with zero counts.
func TestCacheTrimSwitchRecordsNothingUnlessTrimmed(t *testing.T) {
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
// claim is not judged trimmed.
func TestCacheTrimSwitchDoesNotParkAPendingJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobWaitVerb: {stderr: "error: timed out waiting for the condition", code: 1},
		cacheTrimJobStateVerb:     {stdout: `{"status":{"active":1}}`},
	})
	if viaJob(t, f, true, map[string]string{}) {
		t.Fatalf("a pending Job = true, want false; log: %s", f.log.String())
	}
	if f.d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Errorf("a Job still running parked its claim")
	}
	if runs := f.jobRuns(t); len(runs) != 0 {
		t.Errorf("a pending Job recorded %+v, want no run", runs)
	}
}
