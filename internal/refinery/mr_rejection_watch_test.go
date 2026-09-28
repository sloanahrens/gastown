//go:build !windows

package refinery

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

// TestDoMerge_KillsInFlightGateWhenMRRejected guards gt-xp2b4 / gt-55fvl: a
// gate already running when `gt mq reject` closes the MR bead must be killed
// immediately, not left to run to completion and merge a branch the operator
// just rejected. Before this fix, nothing rechecked MR status between gate
// start and merge-push, so a slow gate (make test, minutes in production)
// always won that race — gt-wisp-dz1 was rejected at 16:27:10Z and its
// refinery gate merged+pushed it anyway at 16:36:40Z.
func TestDoMerge_KillsInFlightGateWhenMRRejected(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()
	createFeatureBranch(t, workDir, "feature-reject", "reject.txt", "reject\n")
	commit := run(t, workDir, "git", "rev-parse", "feature-reject")
	pushBranch(t, workDir, "feature-reject")

	store := newPrepushStore(
		prepushIssue("gt-src", ""),
		prepushMRIssue("gt-mr", "feature-reject", "main", "gt-src", commit),
	)
	e := newPrepushEngineer(t, workDir, store)
	e.mrRejectionPollInterval = 10 * time.Millisecond

	// Reject the MR on the first status lookup after the gate has started:
	// the engineer's own pre-gate eligibility recheck must still see it open
	// (or the test proves nothing about mid-gate cancellation), so the
	// rejection waits for the gate's start signal.
	gate := newRejectionGate(t)
	store.beforeGet = func(id string) {
		if id != "gt-mr" || !gate.startedOnce() {
			return
		}
		now := time.Now()
		store.issues["gt-mr"].Status = beadsdk.StatusClosed
		store.issues["gt-mr"].ClosedAt = &now
		store.issues["gt-mr"].UpdatedAt = now
		store.closeReasons["gt-mr"] = "rejected: crew review blocked"
	}

	// A gate that never finishes on its own, so doMerge returning proves the
	// gate was killed (see awaitGateKill for the bound).
	e.config.Gates = map[string]*GateConfig{"test": {Cmd: gate.cmd}}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	mr := &MRInfo{ID: "gt-mr", Branch: "feature-reject", Target: "main", SourceIssue: "gt-src", CommitSHA: commit}
	result := awaitGateKill(t, gate, func() ProcessResult {
		return e.doMerge(context.Background(), mr)
	})

	if result.Success {
		t.Fatalf("expected merge to be refused, got success: %+v", result)
	}
	// The refusal must be the rejection, reported through the mid-run
	// recheck, not some other gate failure.
	if !strings.Contains(result.Error, "status is closed") {
		t.Fatalf("result.Error = %q, want the mid-gate rejection reported", result.Error)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main changed: before %s after %s", before, after)
	}
	if got := store.issues["gt-mr"].Status; got != beadsdk.StatusClosed {
		t.Fatalf("MR status = %s, want closed (left as the rejection set it)", got)
	}
}

// TestVerifyAndPush_KillsInFlightStackGateWhenMemberRejected is the batch-path
// analog of TestDoMerge_KillsInFlightGateWhenMRRejected: a member rejected
// while the stack-tip gate is running must kill that gate immediately rather
// than let it finish and push the whole stack, rejected member included
// (gt-xp2b4). Exercises verifyAndPush directly (as
// TestBatchPush_EditorialRequired_OneMissingNote_RefusesWholeBatchPush does)
// so the assertion is about the gate-watch wiring, not ProcessBatch's
// separate retry/bisection policy for an ordinary test failure.
func TestVerifyAndPush_KillsInFlightStackGateWhenMemberRejected(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()
	createFeatureBranch(t, workDir, "feature-a", "a.txt", "a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "b\n")
	commitA := run(t, workDir, "git", "rev-parse", "feature-a")
	commitB := run(t, workDir, "git", "rev-parse", "feature-b")
	pushBranch(t, workDir, "feature-a")
	pushBranch(t, workDir, "feature-b")

	store := newPrepushStore(
		prepushIssue("gt-src-a", ""),
		prepushIssue("gt-src-b", ""),
		prepushMRIssue("gt-mr-a", "feature-a", "main", "gt-src-a", commitA),
		prepushMRIssue("gt-mr-b", "feature-b", "main", "gt-src-b", commitB),
	)
	e := newPrepushEngineer(t, workDir, store)
	e.mrRejectionPollInterval = 10 * time.Millisecond

	batch := []*MRInfo{
		{ID: "gt-mr-a", Branch: "feature-a", Target: "main", SourceIssue: "gt-src-a", CommitSHA: commitA},
		{ID: "gt-mr-b", Branch: "feature-b", Target: "main", SourceIssue: "gt-src-b", CommitSHA: commitB},
	}
	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("BuildRebaseStack: stacked=%d conflicts=%d err=%v", len(stacked), len(conflicts), err)
	}
	if len(stacked) != 2 {
		t.Fatalf("expected 2 stacked MRs, got %d", len(stacked))
	}

	// Reject gt-mr-b on the watcher's first status lookup after the stack
	// gate has started.
	gate := newRejectionGate(t)
	store.beforeGet = func(id string) {
		if id != "gt-mr-b" || !gate.startedOnce() {
			return
		}
		now := time.Now()
		store.issues["gt-mr-b"].Status = beadsdk.StatusClosed
		store.issues["gt-mr-b"].ClosedAt = &now
		store.issues["gt-mr-b"].UpdatedAt = now
		store.closeReasons["gt-mr-b"] = "rejected: crew review blocked"
	}

	// As in TestDoMerge_KillsInFlightGateWhenMRRejected: the gate never
	// finishes on its own, so verifyAndPush returning proves the kill.
	e.config.Gates = map[string]*GateConfig{"test": {Cmd: gate.cmd}}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	result := awaitGateKill(t, gate, func() *BatchResult {
		return e.verifyAndPush(context.Background(), stacked, "main", nil)
	})

	if len(result.Merged) != 0 {
		t.Fatalf("expected no merged MRs, got %v", result.Merged)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main changed: before %s after %s", before, after)
	}
	// A mid-gate rejection must report as the same clean per-member verdict
	// a rejection landing between steps already gets, not blame the whole
	// stack as bisection "culprits" for a test failure that never ran.
	if len(result.Culprits) != 0 {
		t.Fatalf("expected no culprits (this was a rejection, not a test failure), got %v", mrIDs(result.Culprits))
	}
	if got := store.issues["gt-mr-b"].Status; got != beadsdk.StatusClosed {
		t.Fatalf("rejected MR gt-mr-b status = %s, want closed (left as the rejection set it)", got)
	}
	if got := store.issues["gt-mr-a"].Status; got != beadsdk.StatusOpen {
		t.Fatalf("unaffected MR gt-mr-a status = %s, want still open (never blamed)", got)
	}
}

// gateKillBound is how long a gate may keep running after it has started. It
// is measured from the gate's own start signal, not from the start of the
// merge, so the git work before the gate never counts against it (the old
// whole-call 10s budget did, gt-e9wnj). The watcher polls every 10ms in these
// tests; a minute fails only a kill that is not coming.
const gateKillBound = 60 * time.Second

// rejectionGate is a gate command that never finishes on its own: it writes
// its pid to a FIFO, which blocks until the test reads it, then execs an
// hour-long sleep under that same pid. Reading the FIFO is the test's
// gate-started event; the pid lets a failed test kill the sleep.
type rejectionGate struct {
	cmd      string
	fifo     string
	started  chan struct{}
	rejected sync.Once
	pid      int
}

func newRejectionGate(t *testing.T) *rejectionGate {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "gate-started")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return &rejectionGate{
		cmd:     fmt.Sprintf("echo $$ > %q; exec sleep 3600", fifo),
		fifo:    fifo,
		started: make(chan struct{}),
	}
}

// startedOnce reports true exactly once, on the first call after the gate has
// started, so the store applies the rejection a single time.
func (g *rejectionGate) startedOnce() bool {
	select {
	case <-g.started:
	default:
		return false
	}
	first := false
	g.rejected.Do(func() { first = true })
	return first
}

// awaitGateKill runs call in a goroutine and returns its result. It waits
// without a bound for the gate to start (that covers however long the git work
// before the gate takes), then lets the MR be rejected and gives the gate
// gateKillBound to be killed. On failure it kills the gate's sleep by pid, so
// a regression leaves nothing behind; a passing test never signals a pid.
func awaitGateKill[T any](t *testing.T, g *rejectionGate, call func() T) T {
	t.Helper()
	// A panic in call is reported, not left to crash the binary: after a
	// failed bound, the killed gate sends call down a failure path whose
	// store methods this test's fake does not implement.
	done := make(chan T, 1)
	panicked := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		done <- call()
	}()

	pids := make(chan int, 1)
	go func() {
		f, err := os.Open(g.fifo) // blocks until the gate opens it to write
		if err != nil {
			pids <- 0
			return
		}
		defer f.Close()
		line, _ := bufio.NewReader(f).ReadString('\n')
		pid, _ := strconv.Atoi(strings.TrimSpace(line))
		pids <- pid
	}()
	t.Cleanup(func() {
		if !t.Failed() || g.pid <= 0 {
			return
		}
		if proc, err := os.FindProcess(g.pid); err == nil {
			_ = proc.Kill()
		}
	})

	select {
	case g.pid = <-pids:
	case p := <-panicked:
		t.Fatalf("panicked before the gate started: %v", p)
	case r := <-done:
		// Unblock the reader: a writer opening the FIFO releases its open.
		if w, err := os.OpenFile(g.fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatalf("returned before the gate started: %+v", r)
	}
	close(g.started)

	bound := time.NewTimer(gateKillBound)
	defer bound.Stop()
	select {
	case r := <-done:
		return r
	case p := <-panicked:
		t.Fatalf("panicked while the gate ran: %v", p)
		var zero T
		return zero
	case <-bound.C:
		t.Fatalf("gate still running %v after it started: the rejection watcher did not kill it", gateKillBound)
		var zero T
		return zero
	}
}
