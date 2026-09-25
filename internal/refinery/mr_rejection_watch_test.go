package refinery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	// Reject the MR on the gate's second status lookup: the first is the
	// engineer's own pre-gate eligibility recheck (must still see it open,
	// or the test proves nothing about mid-gate cancellation); the second is
	// the watcher's first poll once the gate is already running.
	getCalls := 0
	store.beforeGet = func(id string) {
		if id != "gt-mr" {
			return
		}
		getCalls++
		if getCalls == 2 {
			now := time.Now()
			store.issues["gt-mr"].Status = beadsdk.StatusClosed
			store.issues["gt-mr"].ClosedAt = &now
			store.issues["gt-mr"].UpdatedAt = now
			store.closeReasons["gt-mr"] = "rejected: crew review blocked"
		}
	}

	// A gate that proves whether it was killed: it only creates GATE_DONE
	// after outliving every reasonable cancellation window. The sleep is
	// generous (well beyond the 10ms poll interval and the process-group
	// kill grace) so the test distinguishes "killed" from "ran to
	// completion" even under heavy parallel test-suite contention, rather
	// than asserting a tight wall-clock bound.
	gateDone := filepath.Join(workDir, "GATE_DONE")
	e.config.Gates = map[string]*GateConfig{
		"test": {Cmd: fmt.Sprintf("sleep 15 && touch %s", gateDone)},
	}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	mr := &MRInfo{ID: "gt-mr", Branch: "feature-reject", Target: "main", SourceIssue: "gt-src", CommitSHA: commit}
	start := time.Now()
	result := e.doMerge(context.Background(), mr)
	elapsed := time.Since(start)

	if result.Success {
		t.Fatalf("expected merge to be refused, got success: %+v", result)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("doMerge took %v — gate was not killed promptly when the MR was rejected mid-run", elapsed)
	}
	if _, err := os.Stat(gateDone); err == nil {
		t.Fatal("gate ran to completion (GATE_DONE exists) instead of being killed on rejection")
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

	// Reject gt-mr-b on its first status lookup, which — since verifyAndPush
	// runs the gate with no pre-check of its own — is the watcher's first
	// poll once the gate is already running.
	getCallsB := 0
	store.beforeGet = func(id string) {
		if id != "gt-mr-b" {
			return
		}
		getCallsB++
		if getCallsB == 1 {
			now := time.Now()
			store.issues["gt-mr-b"].Status = beadsdk.StatusClosed
			store.issues["gt-mr-b"].ClosedAt = &now
			store.issues["gt-mr-b"].UpdatedAt = now
			store.closeReasons["gt-mr-b"] = "rejected: crew review blocked"
		}
	}

	gateDone := filepath.Join(workDir, "GATE_DONE")
	e.config.Gates = map[string]*GateConfig{
		"test": {Cmd: fmt.Sprintf("sleep 15 && touch %s", gateDone)},
	}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	start := time.Now()
	result := e.verifyAndPush(context.Background(), stacked, "main", nil)
	elapsed := time.Since(start)

	if len(result.Merged) != 0 {
		t.Fatalf("expected no merged MRs, got %v", result.Merged)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("verifyAndPush took %v — stack gate was not killed promptly when a member was rejected mid-run", elapsed)
	}
	if _, err := os.Stat(gateDone); err == nil {
		t.Fatal("stack gate ran to completion (GATE_DONE exists) instead of being killed on rejection")
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main changed: before %s after %s", before, after)
	}
}
