package daemon

import (
	"context"
	"testing"
	"time"
)

// TestCacheTrimClaimJobWaitIsTheParameter pins that the trim Job's wait is the
// duration it is given: the wait verb carries that timeout, and a Job still
// running at the bound is reported, not waited on forever.
func TestCacheTrimClaimJobWaitIsTheParameter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wait    time.Duration
		timeout string
		reply   cliReply
		want    bool
	}{
		{90 * time.Second, "--timeout=90s", cliReply{}, true},
		{30 * time.Second, "--timeout=30s", cliReply{stderr: "error: timed out waiting for the condition", code: 1}, false},
		// Sub-second waits are rounded up: kubectl reads --timeout=0s as no
		// limit, so 1 ns and 500 ms must reach it as one whole second.
		{time.Nanosecond, "--timeout=1s", cliReply{}, true},
		{500 * time.Millisecond, "--timeout=1s", cliReply{}, true},
		{1500 * time.Millisecond, "--timeout=2s", cliReply{}, true},
	} {
		verb := "wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " " + tc.timeout
		f := newCacheTrimFixture(t, "", map[string]cliReply{verb: tc.reply})
		finished, err := f.d.cacheTrimWaitJob(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob, tc.wait)
		if err != nil || finished != tc.want {
			t.Errorf("cacheTrimWaitJob with wait %s = %v, %v; want finished %v", tc.wait, finished, err, tc.want)
		}
		if _, ok := f.indexOf(verb); !ok {
			t.Errorf("the wait did not carry the duration it was given: want %q, calls: %v", verb, f.verbs())
		}
	}
}

// TestCacheTrimClaimJobWaitApplyRefusesAPlainWait pins that the apply hands the
// wait its bound and reports the refusal as an error, not as a Job that simply
// did not finish.
func TestCacheTrimClaimJobWaitApplyRefusesAPlainWait(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	m := cacheTrimManifestWithLargeShards(t, 1)
	finished, err := f.d.cacheTrimApply(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob, m, 0)
	if err == nil || finished {
		t.Errorf("cacheTrimApply with a zero wait = %v, %v; want a refusal", finished, err)
	}
}

// TestCacheTrimClaimJobWaitZeroIsRefused pins that a wait of zero or less is
// refused before any cluster call: kubectl reads --timeout=0s as no limit.
func TestCacheTrimClaimJobWaitZeroIsRefused(t *testing.T) {
	t.Parallel()
	for _, wait := range []time.Duration{0, -time.Minute, -time.Nanosecond} {
		f := newCacheTrimFixture(t, "", nil)
		finished, err := f.d.cacheTrimWaitJob(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob, wait)
		if err == nil || finished {
			t.Errorf("cacheTrimWaitJob with wait %s = %v, %v; want a refusal", wait, finished, err)
		}
		if calls := f.verbs(); len(calls) != 0 {
			t.Errorf("cacheTrimWaitJob with wait %s made cluster calls %v; want none", wait, calls)
		}
	}
}
