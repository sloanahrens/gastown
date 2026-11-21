package daemon

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The waits on the trim Job and the log reads a failed Job's last line comes
// from, as the fixture's kubectl seam names them.
const (
	cacheTrimFailWaitCompleteVerb = "wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=1200s"
	cacheTrimFailWaitFailedVerb   = "wait --for=condition=failed job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=1200s"
	cacheTrimFailWaitTrimLogVerb  = "logs job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " -c trim --tail=1"
	cacheTrimFailWaitPlanLogVerb  = "logs job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " -c plan --tail=1"
)

// heldLeg is a kubectl call the fixture holds, as a wait on a condition the Job
// never reaches does. started closes when the call begins; stuck is set when
// the hold ran to the hang guard, which is how a wait that wrongly waits on
// this leg shows: as a failure, not as a hang.
type heldLeg struct {
	started <-chan struct{}
	stuck   *atomic.Bool
}

// await fails the test if the held call never began within the hang guard or
// if the wait under test sat on it until the guard ended it.
func (h heldLeg) await(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	select {
	case <-h.started:
	case <-ctx.Done():
		t.Fatalf("the held leg never started within %s", workspaceHangGuard)
	}
	if h.stuck.Load() {
		t.Fatalf("the wait sat on the held leg for %s: it must end without it", workspaceHangGuard)
	}
}

// cacheTrimFailWaitHold makes the kubectl call for verb hang until the test
// ends or the hang guard does, as a wait on a condition the Job never reaches
// does. The call's start is reported on the returned leg: the fixture records
// a call only when it answers, so a held call is read from the leg, not from
// the recorded calls.
func cacheTrimFailWaitHold(t *testing.T, f *cacheTrimFixture, verb string) heldLeg {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	stuck := &atomic.Bool{}
	t.Cleanup(func() { close(release) })
	answer := f.d.execCmd
	f.d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		if len(cmd.Args) > 1 && cacheTrimVerb(cmd.Args[1:]) == verb {
			once.Do(func() { close(started) })
			ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
			defer cancel()
			select {
			case <-release:
			case <-ctx.Done():
				stuck.Store(true)
			}
			return nil, []byte("error: timed out waiting for the condition\n"), &cliExitError{code: 1}
		}
		return answer(cmd)
	}
	return heldLeg{started: started, stuck: stuck}
}

// TestCacheTrimFailWaitReturnsWhenTheJobFails pins that a Job whose failed
// condition answers at once ends the wait at once, with the failed error naming
// the trim container's last log line. The complete wait is held the whole time,
// as it is for a Job that never completes, so the wait returning shows it did
// not wait on the bound.
func TestCacheTrimFailWaitReturnsWhenTheJobFails(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimFailWaitFailedVerb: {},
		cacheTrimFailWaitTrimLogVerb: {
			stdout: "removing 3 files\ncould not remove /claim/x: permission denied\n",
		},
	})
	held := cacheTrimFailWaitHold(t, f, cacheTrimFailWaitCompleteVerb)
	finished, err := f.d.cacheTrimWaitJob(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob, cacheTrimClaimWaitTimeout)
	if finished || err == nil {
		t.Fatalf("cacheTrimWaitJob = %v, %v; want the failed error", finished, err)
	}
	want := "the trim Job " + cacheTrimJob + " failed: could not remove /claim/x: permission denied"
	if err.Error() != want {
		t.Errorf("error = %q; want %q", err, want)
	}
	if _, ok := f.indexOf(cacheTrimFailWaitFailedVerb); !ok {
		t.Errorf("the failed wait did not run: calls %v", f.verbs())
	}
	if _, ok := f.indexOf(cacheTrimFailWaitPlanLogVerb); ok {
		t.Errorf("the plan container was read though the trim container had a line: calls %v", f.verbs())
	}
	held.await(t)
}

// TestCacheTrimFailWaitNamesOneCutLine pins the shape of the failed error's
// line: at most cacheTrimFailLineLimit characters, and no line at all when
// neither container's log can be read or holds one.
func TestCacheTrimFailWaitNamesOneCutLine(t *testing.T) {
	t.Parallel()
	unreadable := cliReply{stderr: "Error from server: pod is not found\n", code: 1}
	failed := "the trim Job " + cacheTrimJob + " failed"
	for _, tc := range []struct {
		name       string
		trim, plan cliReply
		want       string
	}{
		{"a long line is cut", cliReply{stdout: strings.Repeat("x", 250) + "\n"}, unreadable, failed + ": " + strings.Repeat("x", cacheTrimFailLineLimit)},
		{"neither log holds a line", cliReply{stdout: "\n"}, cliReply{stdout: ""}, failed + "; its logs hold no line"},
		{"neither log can be read", unreadable, unreadable, failed + "; its logs hold no line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, "", map[string]cliReply{
				cacheTrimFailWaitFailedVerb:  {},
				cacheTrimFailWaitTrimLogVerb: tc.trim,
				cacheTrimFailWaitPlanLogVerb: tc.plan,
			})
			held := cacheTrimFailWaitHold(t, f, cacheTrimFailWaitCompleteVerb)
			finished, err := f.d.cacheTrimWaitJob(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob, cacheTrimClaimWaitTimeout)
			if finished || err == nil {
				t.Fatalf("cacheTrimWaitJob = %v, %v; want the failed error", finished, err)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q; want %q", err, tc.want)
			}
			held.await(t)
		})
	}
}

// TestCacheTrimFailWaitReadsThePlanLogAndStillCompletes pins the two outcomes
// the wait's two legs can reach. A Job whose plan init container failed has no
// trim log, so the failed error names the plan container's last line. A Job
// that completes returns true, and the failed wait it leaves running is not
// read.
func TestCacheTrimFailWaitReadsThePlanLogAndStillCompletes(t *testing.T) {
	t.Parallel()
	t.Run("the plan init container failed", func(t *testing.T) {
		f := newCacheTrimFixture(t, "", map[string]cliReply{
			cacheTrimFailWaitFailedVerb: {},
			cacheTrimFailWaitTrimLogVerb: {
				stderr: "Error from server: container \"trim\" is waiting to start: PodInitializing\n",
				code:   1,
			},
			cacheTrimFailWaitPlanLogVerb: {stdout: "the claim list is unreadable\n"},
		})
		held := cacheTrimFailWaitHold(t, f, cacheTrimFailWaitCompleteVerb)
		finished, err := f.d.cacheTrimWaitJob(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob, cacheTrimClaimWaitTimeout)
		if finished || err == nil {
			t.Fatalf("cacheTrimWaitJob = %v, %v; want the failed error", finished, err)
		}
		want := "the trim Job " + cacheTrimJob + " failed: the claim list is unreadable"
		if err.Error() != want {
			t.Errorf("error = %q; want %q", err, want)
		}
		held.await(t)
	})
	t.Run("the Job completes", func(t *testing.T) {
		f := newCacheTrimFixture(t, "", map[string]cliReply{
			cacheTrimFailWaitCompleteVerb: {},
		})
		held := cacheTrimFailWaitHold(t, f, cacheTrimFailWaitFailedVerb)
		finished, err := f.d.cacheTrimWaitJob(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob, cacheTrimClaimWaitTimeout)
		if !finished || err != nil {
			t.Errorf("cacheTrimWaitJob = %v, %v; want finished with no error", finished, err)
		}
		if _, ok := f.indexOf(cacheTrimFailWaitTrimLogVerb); ok {
			t.Errorf("a completed Job's trim log was read: calls %v", f.verbs())
		}
		held.await(t)
	})
}

// cacheTrimFailWaitAnswerAfter holds the answer to verb until started closes, so
// the verb answers only once the other leg's call has begun. The complete wait
// then always has its context recorded before the failed wait's answer lands.
func cacheTrimFailWaitAnswerAfter(f *cacheTrimFixture, verb string, started <-chan struct{}) {
	answer := f.d.execCmd
	f.d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		if len(cmd.Args) > 1 && cacheTrimVerb(cmd.Args[1:]) == verb {
			ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
			defer cancel()
			select {
			case <-started:
			case <-ctx.Done():
			}
		}
		return answer(cmd)
	}
}

// cacheTrimFailWaitCtxs records the context each kubectl call runs under. A wait
// leg runs on a context derived from the parent; a log read runs on the parent
// itself. completeLiveAtLog is set at the first log read: whether the complete
// wait's context was still live then. errsAtCall holds, per call in call order,
// the context's Err at the moment the seam handed it over, before the call ran.
// legs[0] is the complete wait, because the test answers the failed wait only
// once the complete call has begun.
type cacheTrimFailWaitCtxs struct {
	parent            context.Context
	mu                sync.Mutex
	legs              []context.Context
	errsAtCall        []error
	logReads          int
	completeLiveAtLog bool
}

func (c *cacheTrimFailWaitCtxs) record(ctx context.Context, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errsAtCall = append(c.errsAtCall, ctx.Err())
	if ctx != c.parent {
		c.legs = append(c.legs, ctx)
		return
	}
	if c.logReads == 0 && len(c.legs) > 0 {
		c.completeLiveAtLog = c.legs[0].Err() == nil
	}
	c.logReads++
}

// cacheTrimFailWaitRecordCtxs hands the fixture's seam to a recorder for parent.
func cacheTrimFailWaitRecordCtxs(f *cacheTrimFixture, parent context.Context) *cacheTrimFailWaitCtxs {
	c := &cacheTrimFailWaitCtxs{parent: parent}
	f.d.cacheTrimCtxSeen = c.record
	return c
}

// TestCacheTrimFailWaitCancelsTheCompleteLegWhenTheJobFails pins that a Job whose
// failed condition answers cancels the complete wait it leaves held before it
// reads the log, and that both wait legs are canceled once the wait returns.
func TestCacheTrimFailWaitCancelsTheCompleteLegWhenTheJobFails(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimFailWaitFailedVerb:  {},
		cacheTrimFailWaitTrimLogVerb: {stdout: "could not remove /claim/x: permission denied\n"},
	})
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctxs := cacheTrimFailWaitRecordCtxs(f, parent)
	held := cacheTrimFailWaitHold(t, f, cacheTrimFailWaitCompleteVerb)
	cacheTrimFailWaitAnswerAfter(f, cacheTrimFailWaitFailedVerb, held.started)
	finished, err := f.d.cacheTrimWaitJob(parent, "/kubeconfig", cacheTrimNamespace, cacheTrimJob, cacheTrimClaimWaitTimeout)
	if finished || err == nil {
		t.Fatalf("cacheTrimWaitJob = %v, %v; want the failed error", finished, err)
	}
	held.await(t)
	ctxs.mu.Lock()
	defer ctxs.mu.Unlock()
	if ctxs.logReads == 0 {
		t.Fatalf("no log read ran: calls %v", f.verbs())
	}
	if len(ctxs.legs) != 2 {
		t.Fatalf("the pass ran %d wait legs on derived contexts; want the complete and the failed wait", len(ctxs.legs))
	}
	if ctxs.completeLiveAtLog {
		t.Errorf("the complete wait was live when the log was read; want it canceled before the read")
	}
	for i, leg := range ctxs.legs {
		if leg.Err() == nil {
			t.Errorf("wait leg %d is live once the wait returned; want it canceled", i)
		}
	}
}

// TestCacheTrimFailWaitReadsTheLogOnALiveContext pins that every cluster call
// the failed Job's wait makes, and so its log read, runs on a context that is
// live at the moment of the call. The wait cancels its two legs as soon as the
// failed condition answers and reads the log after that; a read handed a leg
// would see a canceled context. The fixture's kubectl ignores its context, so
// only the Err sampled when each call is made shows it.
func TestCacheTrimFailWaitReadsTheLogOnALiveContext(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimFailWaitFailedVerb:  {},
		cacheTrimFailWaitTrimLogVerb: {stdout: "could not remove /claim/x: permission denied\n"},
	})
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctxs := cacheTrimFailWaitRecordCtxs(f, parent)
	held := cacheTrimFailWaitHold(t, f, cacheTrimFailWaitCompleteVerb)
	cacheTrimFailWaitAnswerAfter(f, cacheTrimFailWaitFailedVerb, held.started)
	finished, err := f.d.cacheTrimWaitJob(parent, "/kubeconfig", cacheTrimNamespace, cacheTrimJob, cacheTrimClaimWaitTimeout)
	if finished || err == nil {
		t.Fatalf("cacheTrimWaitJob = %v, %v; want the failed error", finished, err)
	}
	held.await(t)
	if _, ok := f.indexOf(cacheTrimFailWaitTrimLogVerb); !ok {
		t.Fatalf("the failed Job's log was not read: calls %v", f.verbs())
	}
	ctxs.mu.Lock()
	defer ctxs.mu.Unlock()
	if len(ctxs.errsAtCall) == 0 {
		t.Fatalf("the seam saw no cluster call")
	}
	for i, errAtCall := range ctxs.errsAtCall {
		if errAtCall != nil {
			t.Errorf("cluster call %d was made on a context that had already ended (%v); want a live context", i, errAtCall)
		}
	}
	if ctxs.logReads == 0 {
		t.Errorf("no call ran on the parent context; the log read must not run on a canceled leg")
	}
}

// TestCacheTrimFailWaitCancelsTheFailedLegWhenTheJobCompletes pins that a Job
// whose complete condition answers cancels the failed wait it leaves held.
func TestCacheTrimFailWaitCancelsTheFailedLegWhenTheJobCompletes(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimFailWaitCompleteVerb: {},
	})
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctxs := cacheTrimFailWaitRecordCtxs(f, parent)
	held := cacheTrimFailWaitHold(t, f, cacheTrimFailWaitFailedVerb)
	finished, err := f.d.cacheTrimWaitJob(parent, "/kubeconfig", cacheTrimNamespace, cacheTrimJob, cacheTrimClaimWaitTimeout)
	if !finished || err != nil {
		t.Fatalf("cacheTrimWaitJob = %v, %v; want finished with no error", finished, err)
	}
	held.await(t)
	ctxs.mu.Lock()
	defer ctxs.mu.Unlock()
	if len(ctxs.legs) != 2 {
		t.Fatalf("the pass ran %d wait legs on derived contexts; want the complete and the failed wait", len(ctxs.legs))
	}
	for i, leg := range ctxs.legs {
		if leg.Err() == nil {
			t.Errorf("wait leg %d is live once the wait returned; want it canceled", i)
		}
	}
}
