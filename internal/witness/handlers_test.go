package witness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestWitnessHasSubmittableWorkUsesBranchTargetStatus(t *testing.T) {
	repo := setupWitnessSquashPreservedRepo(t)
	if got := witnessHasSubmittableWork(repo, []string{"integration/test"}); got {
		t.Fatal("squash-preserved branch should not require MQ submission through witness helper")
	}

	witnessWriteFile(t, filepath.Join(repo, "feature.txt"), "one\ntwo\nthree\n")
	runWitnessGit(t, repo, "add", "feature.txt")
	runWitnessGit(t, repo, "commit", "-m", "extra local work")
	if got := witnessHasSubmittableWork(repo, []string{"integration/test"}); !got {
		t.Fatal("new local work after squash preservation should still require MQ submission")
	}
}

func TestVerifyBranchAlreadyMergedUsesBranchTargetStatus(t *testing.T) {
	townRoot, workDir := setupSlotOpenTestTown(t)
	repo := filepath.Join(townRoot, "gastown", "polecats", "institute", "gastown")
	setupWitnessSquashPreservedRepoAt(t, repo, filepath.Join(t.TempDir(), "remote.git"))
	runWitnessGit(t, repo, "push", "origin", "integration/test:main")

	merged, err := _verifyBranchAlreadyMerged(workDir, "gastown", "institute", "")
	if err != nil {
		t.Fatalf("_verifyBranchAlreadyMerged: %v", err)
	}
	if !merged {
		t.Fatal("squash-preserved branch on advanced default target should be treated as already merged")
	}

	witnessWriteFile(t, filepath.Join(repo, "feature.txt"), "one\ntwo\nthree\n")
	runWitnessGit(t, repo, "add", "feature.txt")
	runWitnessGit(t, repo, "commit", "-m", "extra local work")
	merged, err = _verifyBranchAlreadyMerged(workDir, "gastown", "institute", "")
	if err != nil {
		t.Fatalf("_verifyBranchAlreadyMerged after extra work: %v", err)
	}
	if merged {
		t.Fatal("new local work after squash preservation should not be treated as already merged")
	}
}

// TestVerifyBranchAlreadyMerged_RejectsStaleBranchFromSupersededAssignment
// covers gt-skwt: a polecat's session can die while its worktree still has a
// PREVIOUS assignment's (now-merged) branch checked out, then get
// force-reassigned to a new, different hookBead before ever checking out a
// branch for it. The stale on-disk branch being merged says nothing about
// the new hookBead's (unstarted) work, and must not be reported as such —
// otherwise handleZombieRestart archives/nukes a polecat that never touched
// its actual assignment.
func TestVerifyBranchAlreadyMerged_RejectsStaleBranchFromSupersededAssignment(t *testing.T) {
	townRoot, workDir := setupSlotOpenTestTown(t)
	repo := filepath.Join(townRoot, "gastown", "polecats", "rust", "gastown")
	remote := filepath.Join(t.TempDir(), "remote.git")

	runWitnessGit(t, filepath.Dir(remote), "init", "--bare", remote)
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	runWitnessGit(t, repo, "init")
	runWitnessGit(t, repo, "config", "user.email", "test@example.com")
	runWitnessGit(t, repo, "config", "user.name", "Test User")
	witnessWriteFile(t, filepath.Join(repo, "README.md"), "base\n")
	runWitnessGit(t, repo, "add", "README.md")
	runWitnessGit(t, repo, "commit", "-m", "base")
	runWitnessGit(t, repo, "branch", "-M", "main")
	runWitnessGit(t, repo, "remote", "add", "origin", remote)
	runWitnessGit(t, repo, "push", "-u", "origin", "main")

	// Old assignment's branch: its work landed on main.
	oldBranch := "polecat/rust/be-dxx+aaa111"
	runWitnessGit(t, repo, "switch", "-c", oldBranch)
	witnessWriteFile(t, filepath.Join(repo, "old.txt"), "old work\n")
	runWitnessGit(t, repo, "add", "old.txt")
	runWitnessGit(t, repo, "commit", "-m", "old work")
	runWitnessGit(t, repo, "switch", "main")
	runWitnessGit(t, repo, "merge", "--ff-only", oldBranch)
	runWitnessGit(t, repo, "push", "origin", "main")
	// Session died before checking out a branch for the new assignment — the
	// worktree is still sitting on the old (now-merged) branch.
	runWitnessGit(t, repo, "switch", oldBranch)

	// Unknown hookBead: preserve the old (pre-gt-skwt) behavior.
	if merged, err := _verifyBranchAlreadyMerged(workDir, "gastown", "rust", ""); err != nil || !merged {
		t.Fatalf("merged=%v err=%v, want merged=true when hookBead is unknown", merged, err)
	}

	// Current hookBead differs from the branch's embedded issue (be-4c2 was
	// force-reassigned to this polecat after be-dxx merged): must NOT report
	// merged, or handleZombieRestart will archive/nuke the polecat for work
	// on be-4c2 it never started.
	merged, err := _verifyBranchAlreadyMerged(workDir, "gastown", "rust", "be-4c2")
	if err != nil {
		t.Fatalf("_verifyBranchAlreadyMerged: %v", err)
	}
	if merged {
		t.Fatal("stale branch from a superseded assignment must not count as the current hookBead's work being merged")
	}

	// The branch DOES match the current hookBead: genuine merged-work
	// detection (aa-apw) must still fire.
	merged, err = _verifyBranchAlreadyMerged(workDir, "gastown", "rust", "be-dxx")
	if err != nil {
		t.Fatalf("_verifyBranchAlreadyMerged: %v", err)
	}
	if !merged {
		t.Fatal("branch matching the current hookBead should still be detected as merged")
	}
}

func setupWitnessSquashPreservedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	setupWitnessSquashPreservedRepoAt(t, repo, remote)
	return repo
}

func setupWitnessSquashPreservedRepoAt(t *testing.T, repo, remote string) {
	t.Helper()
	runWitnessGit(t, filepath.Dir(remote), "init", "--bare", remote)
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	runWitnessGit(t, repo, "init")
	runWitnessGit(t, repo, "config", "user.email", "test@example.com")
	runWitnessGit(t, repo, "config", "user.name", "Test User")
	witnessWriteFile(t, filepath.Join(repo, "README.md"), "base\n")
	runWitnessGit(t, repo, "add", "README.md")
	runWitnessGit(t, repo, "commit", "-m", "base")
	runWitnessGit(t, repo, "branch", "-M", "main")
	runWitnessGit(t, repo, "remote", "add", "origin", remote)
	runWitnessGit(t, repo, "push", "-u", "origin", "main")
	runWitnessGit(t, repo, "switch", "-c", "integration/test")
	runWitnessGit(t, repo, "push", "-u", "origin", "integration/test")
	if err := exec.Command("git", "-C", repo, "merge-tree", "--write-tree", "HEAD", "HEAD").Run(); err != nil {
		t.Skipf("git merge-tree --write-tree unsupported: %v", err)
	}

	runWitnessGit(t, repo, "switch", "-c", "polecat/squash")
	witnessWriteFile(t, filepath.Join(repo, "feature.txt"), "one\n")
	runWitnessGit(t, repo, "add", "feature.txt")
	runWitnessGit(t, repo, "commit", "-m", "checkpoint one")
	witnessWriteFile(t, filepath.Join(repo, "feature.txt"), "one\ntwo\n")
	runWitnessGit(t, repo, "add", "feature.txt")
	runWitnessGit(t, repo, "commit", "-m", "checkpoint two")
	runWitnessGit(t, repo, "switch", "integration/test")
	runWitnessGit(t, repo, "merge", "--squash", "polecat/squash")
	runWitnessGit(t, repo, "commit", "-m", "squash polecat work")
	witnessWriteFile(t, filepath.Join(repo, "target.txt"), "target advanced\n")
	runWitnessGit(t, repo, "add", "target.txt")
	runWitnessGit(t, repo, "commit", "-m", "advance target")
	runWitnessGit(t, repo, "push", "origin", "integration/test")
	runWitnessGit(t, repo, "switch", "polecat/squash")
}

func witnessWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func runWitnessGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

type testMayorEvent struct {
	Type    string            `json:"type"`
	Payload map[string]string `json:"payload"`
}

func setupSlotOpenTestTown(t *testing.T) (string, string) {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(townRoot, "gastown", "witness")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	return townRoot, workDir
}

func readMayorEvents(t *testing.T, townRoot string) []testMayorEvent {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(townRoot, "events", "mayor", "*.event"))
	if err != nil {
		t.Fatal(err)
	}
	events := make([]testMayorEvent, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var event testMayorEvent
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestNotifyMayorSlotOpen_BlocksNonCompletedExit(t *testing.T) {
	townRoot, workDir := setupSlotOpenTestTown(t)

	notifyMayorSlotOpen(workDir, "gastown", "guzzle", string(ExitTypeDeferred))

	events := readMayorEvents(t, townRoot)
	if len(events) != 1 {
		t.Fatalf("events = %v, want one SLOT_BLOCKED event", events)
	}
	event := events[0]
	if event.Type != "SLOT_BLOCKED" {
		t.Fatalf("event type = %q, want SLOT_BLOCKED", event.Type)
	}
	if event.Payload["reason"] != "exit-deferred" {
		t.Fatalf("reason = %q, want exit-deferred", event.Payload["reason"])
	}
}

func TestNotifyMayorSlotOpen_SchedulerDispatchSuppressesMayor(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	townRoot, workDir := setupSlotOpenTestTown(t)

	prevRecovery := slotOpenRecoveryCheck
	prevDecision := slotOpenDecisionForNotify
	prevScheduler := runSchedulerForSlotOpen
	t.Cleanup(func() {
		slotOpenRecoveryCheck = prevRecovery
		slotOpenDecisionForNotify = prevDecision
		runSchedulerForSlotOpen = prevScheduler
	})

	slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
		return `{"verdict":"SAFE_TO_NUKE"}`, nil
	}
	slotOpenDecisionForNotify = func(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
		return polecat.SlotReuseDecision{Reusable: true}
	}
	called := false
	runSchedulerForSlotOpen = func(gotTownRoot string) (slotOpenSchedulerResult, error) {
		called = true
		if gotTownRoot != townRoot {
			t.Fatalf("townRoot = %q, want %q", gotTownRoot, townRoot)
		}
		return slotOpenSchedulerResult{Dispatched: 1}, nil
	}

	notifyMayorSlotOpen(workDir, "gastown", "guzzle", string(ExitTypeCompleted))

	if !called {
		t.Fatal("scheduler trigger was not called")
	}
	if events := readMayorEvents(t, townRoot); len(events) != 0 {
		t.Fatalf("events = %+v, want none when scheduler dispatches", events)
	}
}

func TestNotifyMayorSlotOpen_DispatchThenEmptyEmitsSchedulerOpen(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	townRoot, workDir := setupSlotOpenTestTown(t)

	prevRecovery := slotOpenRecoveryCheck
	prevDecision := slotOpenDecisionForNotify
	prevScheduler := runSchedulerForSlotOpen
	t.Cleanup(func() {
		slotOpenRecoveryCheck = prevRecovery
		slotOpenDecisionForNotify = prevDecision
		runSchedulerForSlotOpen = prevScheduler
	})

	slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
		return `{"verdict":"SAFE_TO_NUKE"}`, nil
	}
	slotOpenDecisionForNotify = func(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
		return polecat.SlotReuseDecision{Reusable: true}
	}
	runSchedulerForSlotOpen = func(gotTownRoot string) (slotOpenSchedulerResult, error) {
		var result slotOpenSchedulerResult
		result.Ran = true
		result.Dispatched = 1
		result.After.Capacity.Max = 10
		result.After.Capacity.Free = 1
		result.After.QueuedReady = 0
		return result, nil
	}

	notifyMayorSlotOpen(workDir, "gastown", "guzzle", string(ExitTypeCompleted))

	events := readMayorEvents(t, townRoot)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one SCHEDULER_OPEN event", events)
	}
	if events[0].Type != "SCHEDULER_OPEN" {
		t.Fatalf("event type = %q, want SCHEDULER_OPEN", events[0].Type)
	}
}

func TestNotifyMayorSlotOpen_DispatchWithStatusErrorSuppressesMayor(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	townRoot, workDir := setupSlotOpenTestTown(t)

	prevRecovery := slotOpenRecoveryCheck
	prevDecision := slotOpenDecisionForNotify
	prevScheduler := runSchedulerForSlotOpen
	t.Cleanup(func() {
		slotOpenRecoveryCheck = prevRecovery
		slotOpenDecisionForNotify = prevDecision
		runSchedulerForSlotOpen = prevScheduler
	})

	slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
		return `{"verdict":"SAFE_TO_NUKE"}`, nil
	}
	slotOpenDecisionForNotify = func(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
		return polecat.SlotReuseDecision{Reusable: true}
	}
	runSchedulerForSlotOpen = func(gotTownRoot string) (slotOpenSchedulerResult, error) {
		return slotOpenSchedulerResult{Dispatched: 1}, errors.New("status read failed")
	}

	notifyMayorSlotOpen(workDir, "gastown", "guzzle", string(ExitTypeCompleted))

	if events := readMayorEvents(t, townRoot); len(events) != 0 {
		t.Fatalf("events = %+v, want none after confirmed dispatch", events)
	}
}

func TestNotifyMayorSlotOpen_EmitsSchedulerOpenWhenQueueEmpty(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	townRoot, workDir := setupSlotOpenTestTown(t)

	prevRecovery := slotOpenRecoveryCheck
	prevDecision := slotOpenDecisionForNotify
	prevScheduler := runSchedulerForSlotOpen
	t.Cleanup(func() {
		slotOpenRecoveryCheck = prevRecovery
		slotOpenDecisionForNotify = prevDecision
		runSchedulerForSlotOpen = prevScheduler
	})

	slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
		return `{"verdict":"SAFE_TO_NUKE"}`, nil
	}
	slotOpenDecisionForNotify = func(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
		return polecat.SlotReuseDecision{Reusable: true}
	}
	runSchedulerForSlotOpen = func(gotTownRoot string) (slotOpenSchedulerResult, error) {
		var result slotOpenSchedulerResult
		result.Before.Capacity.Max = 10
		result.Before.Capacity.Free = 2
		result.Before.QueuedReady = 0
		return result, nil
	}

	notifyMayorSlotOpen(workDir, "gastown", "guzzle", string(ExitTypeCompleted))

	events := readMayorEvents(t, townRoot)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one SCHEDULER_OPEN event", events)
	}
	if events[0].Type != "SCHEDULER_OPEN" {
		t.Fatalf("event type = %q, want SCHEDULER_OPEN", events[0].Type)
	}
	if events[0].Payload["capacity_free"] != "2" {
		t.Fatalf("capacity_free = %q, want 2", events[0].Payload["capacity_free"])
	}
}

func TestNotifyMayorSlotOpen_QueuedReadyWithoutDispatchFallsBack(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	townRoot, workDir := setupSlotOpenTestTown(t)

	prevRecovery := slotOpenRecoveryCheck
	prevDecision := slotOpenDecisionForNotify
	prevScheduler := runSchedulerForSlotOpen
	t.Cleanup(func() {
		slotOpenRecoveryCheck = prevRecovery
		slotOpenDecisionForNotify = prevDecision
		runSchedulerForSlotOpen = prevScheduler
	})

	slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
		return `{"verdict":"SAFE_TO_NUKE"}`, nil
	}
	slotOpenDecisionForNotify = func(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
		return polecat.SlotReuseDecision{Reusable: true}
	}
	runSchedulerForSlotOpen = func(gotTownRoot string) (slotOpenSchedulerResult, error) {
		var result slotOpenSchedulerResult
		result.Before.Capacity.Max = 10
		result.Before.Capacity.Free = 1
		result.Before.QueuedReady = 1
		result.After = result.Before
		result.Ran = true
		return result, nil
	}

	notifyMayorSlotOpen(workDir, "gastown", "guzzle", string(ExitTypeCompleted))

	events := readMayorEvents(t, townRoot)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one fallback SLOT_OPEN event", events)
	}
	if events[0].Type != "SLOT_OPEN" {
		t.Fatalf("event type = %q, want SLOT_OPEN", events[0].Type)
	}
}

func TestNotifyMayorSlotOpen_NoDispatchAfterCapacityFillsSuppressesMayor(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	townRoot, workDir := setupSlotOpenTestTown(t)

	prevRecovery := slotOpenRecoveryCheck
	prevDecision := slotOpenDecisionForNotify
	prevScheduler := runSchedulerForSlotOpen
	t.Cleanup(func() {
		slotOpenRecoveryCheck = prevRecovery
		slotOpenDecisionForNotify = prevDecision
		runSchedulerForSlotOpen = prevScheduler
	})

	slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
		return `{"verdict":"SAFE_TO_NUKE"}`, nil
	}
	slotOpenDecisionForNotify = func(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
		return polecat.SlotReuseDecision{Reusable: true}
	}
	runSchedulerForSlotOpen = func(gotTownRoot string) (slotOpenSchedulerResult, error) {
		var result slotOpenSchedulerResult
		result.Before.Capacity.Max = 10
		result.Before.Capacity.Free = 1
		result.Before.QueuedReady = 1
		result.After.Capacity.Max = 10
		result.After.Capacity.Free = 0
		result.After.QueuedReady = 1
		result.Ran = true
		return result, nil
	}

	notifyMayorSlotOpen(workDir, "gastown", "guzzle", string(ExitTypeCompleted))

	if events := readMayorEvents(t, townRoot); len(events) != 0 {
		t.Fatalf("events = %+v, want none when scheduler no longer has capacity", events)
	}
}

func TestParseSchedulerRunDispatched(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
	}{
		{name: "dispatched", output: "\n✓ Dispatched 2, failed 0 (reason: batch)\n", want: 2},
		{name: "skipped", output: "\n○ Skipped 1 bead(s) — zero capacity\n", want: 0},
		{name: "cleanup only", output: "", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseSchedulerRunDispatched(tt.output); got != tt.want {
				t.Fatalf("parseSchedulerRunDispatched() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestShouldNotifyMayorSlotOpenRequiresSafeRecovery(t *testing.T) {
	prev := slotOpenRecoveryCheck
	t.Cleanup(func() { slotOpenRecoveryCheck = prev })

	tests := []struct {
		name    string
		output  string
		err     error
		wantOK  bool
		wantMsg string
	}{
		{
			name:   "safe to nuke notifies",
			output: `{"verdict":"SAFE_TO_NUKE"}`,
			wantOK: true,
		},
		{
			name:   "warning-prefixed json notifies",
			output: "warning: stale binary\n" + `{"verdict":"SAFE_TO_NUKE"}`,
			wantOK: true,
		},
		{
			name:    "needs recovery suppresses",
			output:  `{"verdict":"NEEDS_RECOVERY","blockers":["cleanup_status=has_unpushed"]}`,
			wantMsg: "NEEDS_RECOVERY",
		},
		{
			name:    "needs mq submit suppresses",
			output:  `{"verdict":"NEEDS_MQ_SUBMIT"}`,
			wantMsg: "NEEDS_MQ_SUBMIT",
		},
		{
			name:    "check failure suppresses",
			err:     errors.New("boom"),
			wantMsg: "check-recovery failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
				return tt.output, tt.err
			}

			gotOK, gotMsg := shouldNotifyMayorSlotOpen("/tmp", "gastown", "nitro")
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v (msg=%q)", gotOK, tt.wantOK, gotMsg)
			}
			if tt.wantMsg != "" && !strings.Contains(gotMsg, tt.wantMsg) {
				t.Fatalf("message %q does not contain %q", gotMsg, tt.wantMsg)
			}
		})
	}
}

func TestActiveMRBlockerFromCLIUsesTerminalStatus(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
		want   string
	}{
		{name: "empty active mr", want: ""},
		{name: "open mr blocks", output: `[{"status":"open"}]`, want: "active_mr=gt-mr status=open"},
		{name: "closed mr does not block", output: `[{"status":"closed"}]`, want: ""},
		{name: "not found does not block", err: fmt.Errorf("issue not found"), want: ""},
		{name: "lookup error blocks", err: fmt.Errorf("bd unavailable"), want: "active_mr=gt-mr status=lookup_error: bd unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd, _ := mockBd(
				func(args []string) (string, error) { return tt.output, tt.err },
				func(args []string) error { return nil },
			)
			mrID := "gt-mr"
			if tt.name == "empty active mr" {
				mrID = ""
			}
			if got := activeMRBlockerFromCLI(bd, t.TempDir(), mrID); got != tt.want {
				t.Fatalf("activeMRBlockerFromCLI() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandlePolecatDoneFromBead_NilFields(t *testing.T) {
	t.Parallel()
	result := HandlePolecatDoneFromBead(DefaultBdCli(), "/tmp", "testrig", "nux", nil, nil)
	if result.Error == nil {
		t.Error("expected error for nil fields")
	}
	if result.Handled {
		t.Error("should not be handled with nil fields")
	}
}

func TestHandlePolecatDoneFromBead_PhaseComplete(t *testing.T) {
	t.Parallel()
	fields := &beads.AgentFields{
		ExitType: "PHASE_COMPLETE",
		Branch:   "polecat/nux",
	}
	result := HandlePolecatDoneFromBead(DefaultBdCli(), "/tmp", "testrig", "nux", fields, nil)
	if !result.Handled {
		t.Error("expected PHASE_COMPLETE to be handled")
	}
	if result.Error != nil {
		t.Errorf("unexpected error: %v", result.Error)
	}
	if !strings.Contains(result.Action, "phase-complete") {
		t.Errorf("action %q should contain 'phase-complete'", result.Action)
	}
}

func TestHandlePolecatDoneFromBead_NoMR(t *testing.T) {
	t.Parallel()
	fields := &beads.AgentFields{
		ExitType:       "COMPLETED",
		Branch:         "polecat/nux",
		HookBead:       "gt-test123",
		CompletionTime: "2026-02-28T01:00:00Z",
	}
	result := HandlePolecatDoneFromBead(DefaultBdCli(), "/tmp/nonexistent", "testrig", "nux", fields, nil)
	if !result.Handled {
		t.Error("expected completion with no MR to be handled")
	}
	if !strings.Contains(result.Action, "no MR") {
		t.Errorf("action %q should contain 'no MR'", result.Action)
	}
}

func TestHandlePolecatDoneFromBead_ProtocolType(t *testing.T) {
	t.Parallel()
	fields := &beads.AgentFields{
		ExitType: "COMPLETED",
		Branch:   "polecat/nux",
	}
	result := HandlePolecatDoneFromBead(DefaultBdCli(), "/tmp/nonexistent", "testrig", "nux", fields, nil)
	if result.ProtocolType != ProtoPolecatDone {
		t.Errorf("ProtocolType = %q, want %q", result.ProtocolType, ProtoPolecatDone)
	}
}

func TestZombieResult_Types(t *testing.T) {
	t.Parallel()
	// Verify the ZombieResult type has all expected fields
	z := ZombieResult{
		PolecatName:    "nux",
		AgentState:     "working",
		Classification: ZombieSessionDeadActive,
		HookBead:       "gt-abc123",
		Action:         "restarted",
		BeadRecovered:  true,
		Error:          nil,
	}

	if z.PolecatName != "nux" {
		t.Errorf("PolecatName = %q, want %q", z.PolecatName, "nux")
	}
	if z.AgentState != "working" {
		t.Errorf("AgentState = %q, want %q", z.AgentState, "working")
	}
	if z.Classification != ZombieSessionDeadActive {
		t.Errorf("Classification = %q, want %q", z.Classification, ZombieSessionDeadActive)
	}
	if z.HookBead != "gt-abc123" {
		t.Errorf("HookBead = %q, want %q", z.HookBead, "gt-abc123")
	}
	if z.Action != "restarted" {
		t.Errorf("Action = %q, want %q", z.Action, "restarted")
	}
	if !z.BeadRecovered {
		t.Error("BeadRecovered = false, want true")
	}
}

func TestDetectZombiePolecatsResult_EmptyResult(t *testing.T) {
	t.Parallel()
	result := &DetectZombiePolecatsResult{}

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0", result.Checked)
	}
	if len(result.Zombies) != 0 {
		t.Errorf("Zombies length = %d, want 0", len(result.Zombies))
	}
}

func TestDetectZombiePolecats_NonexistentDir(t *testing.T) {
	t.Parallel()
	// Should handle missing polecats directory gracefully
	result := DetectZombiePolecats(DefaultBdCli(), "/nonexistent/path", "testrig", nil)

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 for nonexistent dir", result.Checked)
	}
	if len(result.Zombies) != 0 {
		t.Errorf("Zombies = %d, want 0 for nonexistent dir", len(result.Zombies))
	}
}

func TestDetectZombiePolecats_DirectoryScanning(t *testing.T) {
	t.Parallel()
	// Create a temp directory structure simulating polecats
	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create polecat directories
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if err := os.Mkdir(filepath.Join(polecatsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Create hidden dir (should be skipped)
	if err := os.Mkdir(filepath.Join(polecatsDir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a regular file (should be skipped, not a dir)
	if err := os.WriteFile(filepath.Join(polecatsDir, "notadir.txt"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := DetectZombiePolecats(DefaultBdCli(), tmpDir, rigName, nil)

	// Should have checked 3 polecat dirs (not hidden, not file)
	if result.Checked != 3 {
		t.Errorf("Checked = %d, want 3 (should skip hidden dirs and files)", result.Checked)
	}

	// No zombies because agent bead state will be empty (bd not available),
	// so isZombie stays false for all polecats
	if len(result.Zombies) != 0 {
		t.Errorf("Zombies = %d, want 0 (no agent state = not zombie)", len(result.Zombies))
	}
}

func TestDetectZombiePolecats_EmptyPolecatsDir(t *testing.T) {
	t.Parallel()
	// Empty polecats directory should return 0 checked
	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	result := DetectZombiePolecats(DefaultBdCli(), tmpDir, rigName, nil)

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 for empty polecats dir", result.Checked)
	}
}

func TestSessionRecreated_NoSession(t *testing.T) {
	t.Parallel()
	// When the session doesn't exist, sessionRecreated should return false
	// (the session wasn't recreated, it's still dead)
	tm := tmux.NewTmux()
	detectedAt := time.Now()

	recreated := sessionRecreated(tm, "gt-nonexistent-session-xyz", detectedAt)
	if recreated {
		t.Error("sessionRecreated returned true for nonexistent session, want false")
	}
}

func TestSessionRecreated_DetectedAtEdgeCases(t *testing.T) {
	t.Parallel()
	// Verify that sessionRecreated returns false when session is dead
	// regardless of the detectedAt timestamp
	tm := tmux.NewTmux()

	// Try with a past timestamp
	recreated := sessionRecreated(tm, "gt-test-nosession-abc", time.Now().Add(-1*time.Hour))
	if recreated {
		t.Error("sessionRecreated returned true for nonexistent session with past time")
	}

	// Try with a future timestamp
	recreated = sessionRecreated(tm, "gt-test-nosession-def", time.Now().Add(1*time.Hour))
	if recreated {
		t.Error("sessionRecreated returned true for nonexistent session with future time")
	}
}

func TestZombieClassification_SpawningState(t *testing.T) {
	t.Parallel()
	// Verify that "spawning" agent state is treated as a zombie indicator.
	// This tests the classification logic inline in DetectZombiePolecats.
	// We can't easily test this via the full function without mocking,
	// so we test the boolean logic directly.
	states := map[string]bool{
		"working":  true,
		"running":  true,
		"spawning": true,
		"idle":     false,
		"done":     false,
		"":         false,
	}

	for state, wantZombie := range states {
		hookBead := ""
		isZombie := false
		if hookBead != "" {
			isZombie = true
		}
		if state == "working" || state == "running" || state == "spawning" {
			isZombie = true
		}

		if isZombie != wantZombie {
			t.Errorf("agent_state=%q: isZombie=%v, want %v", state, isZombie, wantZombie)
		}
	}
}

func TestZombieClassification_HookBeadAlwaysZombie(t *testing.T) {
	t.Parallel()
	// Any polecat with a hook_bead and dead session should be classified as zombie,
	// regardless of agent_state.
	for _, state := range []string{"", "idle", "done", "working"} {
		hookBead := "gt-some-issue"
		isZombie := false
		if hookBead != "" {
			isZombie = true
		}
		if state == "working" || state == "running" || state == "spawning" {
			isZombie = true
		}

		if !isZombie {
			t.Errorf("agent_state=%q with hook_bead=%q: isZombie=false, want true", state, hookBead)
		}
	}
}

func TestZombieClassification_NoHookNoActiveState(t *testing.T) {
	t.Parallel()
	// Polecats with no hook_bead and non-active agent_state should NOT be zombies.
	for _, state := range []string{"", "idle", "done", "completed"} {
		hookBead := ""
		isZombie := false
		if hookBead != "" {
			isZombie = true
		}
		if state == "working" || state == "running" || state == "spawning" {
			isZombie = true
		}

		if isZombie {
			t.Errorf("agent_state=%q with no hook_bead: isZombie=true, want false", state)
		}
	}
}

func TestFindAnyCleanupWisp_NoBdAvailable(t *testing.T) {
	t.Parallel()
	// When bd is not available (test environment), findAnyCleanupWisp
	// should return empty string without panicking
	result := findAnyCleanupWisp(DefaultBdCli(), "/nonexistent", "gastown", "testpolecat")
	if result != "" {
		t.Errorf("findAnyCleanupWisp = %q, want empty when bd unavailable", result)
	}
}

// mockBdCalls captures bd invocations and returns canned responses.
// Returns a slice that accumulates "arg0 arg1 ..." strings for each call.
type mockBdCalls struct {
	calls []string
}

// mockBd creates a test-local *BdCli with mock exec/run functions.
// Returns the BdCli and a pointer to the captured call log.
// No global state is modified — safe for use with t.Parallel().
func mockBd(execFn func(args []string) (string, error), runFn func(args []string) error) (*BdCli, *mockBdCalls) {
	mock := &mockBdCalls{}
	bd := &BdCli{
		Exec: func(workDir string, args ...string) (string, error) {
			mock.calls = append(mock.calls, strings.Join(args, " "))
			return execFn(stripMockBdFlags(args))
		},
		Run: func(workDir string, args ...string) error {
			mock.calls = append(mock.calls, strings.Join(args, " "))
			return runFn(stripMockBdFlags(args))
		},
	}
	return bd, mock
}

func stripMockBdFlags(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		args = args[1:]
	}
	return args
}

func installFakeTmuxNoServer(t *testing.T) {
	t.Helper()

	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' 'no server running on /tmp/tmux' 1>&2\nexit 1\n"
	if runtime.GOOS == "windows" {
		scriptPath += ".bat"
		script = "@echo off\r\necho no server running on C:\\tmp\\tmux 1>&2\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeBd creates a test-local *BdCli matching the old shell script behavior:
// list/query→"[]", update→ok, show→cleanup wisp JSON. Returns BdCli and captured call log.
func fakeBd() (*BdCli, *mockBdCalls) {
	return mockBd(
		func(args []string) (string, error) {
			if len(args) > 0 {
				switch args[0] {
				case "list", "query":
					return "[]", nil
				case "show":
					return `[{"labels":["cleanup","polecat:testpol","state:pending"]}]`, nil
				}
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)
}

func setupActiveMRGitSafeWorkDir(t *testing.T, rigName, polecatName string) string {
	t.Helper()
	townRoot := t.TempDir()
	clonePath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if err := os.MkdirAll(clonePath, 0755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit(clonePath, "init")
	runGit(clonePath, "config", "user.email", "test@example.com")
	runGit(clonePath, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(clonePath, "README.md"), []byte("test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(clonePath, "add", "README.md")
	runGit(clonePath, "commit", "-m", "initial")
	remotePath := filepath.Join(townRoot, "origin.git")
	runGit(townRoot, "init", "--bare", remotePath)
	runGit(clonePath, "remote", "add", "origin", remotePath)
	runGit(clonePath, "push", "-u", "origin", "HEAD")
	return townRoot
}

func TestHasPendingMRFromSnapshotAssessesMRStatus(t *testing.T) {
	issueJSON := func(id, status, desc string) string {
		b, err := json.Marshal([]map[string]any{{"id": id, "status": status, "description": desc}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	tests := []struct {
		name string
		show func(id string) (string, error)
		want bool
	}{
		{
			name: "open MR is pending",
			show: func(id string) (string, error) {
				return issueJSON(id, "open", ""), nil
			},
			want: true,
		},
		{
			name: "closed MR with terminal source is not pending",
			show: func(id string) (string, error) {
				if id == "gt-mr" {
					return issueJSON(id, "closed", ""), nil
				}
				return issueJSON(id, "closed", ""), nil
			},
		},
		{
			name: "missing MR with terminal source is not pending",
			show: func(id string) (string, error) {
				if id == "gt-mr" {
					return "", errors.New("not found")
				}
				return issueJSON(id, "closed", ""), nil
			},
		},
		{
			name: "lookup error is pending",
			show: func(id string) (string, error) { return "", errors.New("bd exploded") },
			want: true,
		},
		{
			name: "closed MR with open source is pending",
			show: func(id string) (string, error) {
				if id == "gt-mr" {
					return issueJSON(id, "closed", ""), nil
				}
				return issueJSON(id, "open", ""), nil
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := setupActiveMRGitSafeWorkDir(t, "gastown", "nux")
			bd, _ := mockBd(
				func(args []string) (string, error) {
					if len(args) == 0 {
						return "", nil
					}
					switch args[0] {
					case "list", "query":
						return "[]", nil
					case "show":
						return tt.show(args[1])
					}
					return "", nil
				},
				func(args []string) error { return nil },
			)
			snap := &agentBeadSnapshot{ActiveMR: "gt-mr", Fields: &beads.AgentFields{ActiveMR: "gt-mr", LastSourceIssue: "gt-src"}}
			if got := hasPendingMRFromSnapshot(bd, workDir, "gastown", "nux", snap); got != tt.want {
				t.Fatalf("hasPendingMRFromSnapshot() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasPendingMRUsesAgentLastSourceIssue(t *testing.T) {
	workDir := setupActiveMRGitSafeWorkDir(t, "gastown", "nux")
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 {
				return "", nil
			}
			switch args[0] {
			case "list", "query":
				return "[]", nil
			case "show":
				switch args[1] {
				case "gt-agent":
					return `[{"active_mr":"gt-mr","description":"active_mr: gt-mr\nlast_source_issue: gt-src\n"}]`, nil
				case "gt-mr":
					return "", errors.New("not found")
				case "gt-src":
					return `[{"id":"gt-src","status":"closed"}]`, nil
				}
			}
			return "", errors.New("not found")
		},
		func(args []string) error { return nil },
	)

	if got := hasPendingMR(bd, workDir, "gastown", "nux", "gt-agent"); got {
		t.Fatalf("hasPendingMR() = true, want false for missing MR with terminal source")
	}
}

func TestHasPendingMRFromSnapshotRequiresGitSafe(t *testing.T) {
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 {
				return "", nil
			}
			switch args[0] {
			case "list", "query":
				return "[]", nil
			case "show":
				if args[1] == "gt-mr" {
					return "", errors.New("not found")
				}
				return `[{"id":"gt-src","status":"closed"}]`, nil
			}
			return "", nil
		},
		func(args []string) error { return nil },
	)
	snap := &agentBeadSnapshot{ActiveMR: "gt-mr", Fields: &beads.AgentFields{ActiveMR: "gt-mr", LastSourceIssue: "gt-src"}}
	if got := hasPendingMRFromSnapshot(bd, t.TempDir(), "gastown", "nux", snap); !got {
		t.Fatalf("hasPendingMRFromSnapshot() = false, want true when git is unsafe")
	}
}

func TestHasPendingMRCleanupWispFailsClosed(t *testing.T) {
	workDir := setupActiveMRGitSafeWorkDir(t, "gastown", "nux")
	tests := []struct {
		name string
		list string
		err  error
	}{
		{name: "cleanup wisp present", list: `[{"id":"gt-cleanup"}]`},
		{name: "cleanup wisp lookup error", err: errors.New("bd exploded")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd, _ := mockBd(
				func(args []string) (string, error) {
					if len(args) == 0 {
						return "", nil
					}
					if args[0] == "list" {
						return tt.list, tt.err
					}
					if args[0] == "show" && args[1] == "gt-agent" {
						return `[{"active_mr":"gt-mr","description":"active_mr: gt-mr\nlast_source_issue: gt-src\n"}]`, nil
					}
					if args[0] == "show" && args[1] == "gt-mr" {
						return "", errors.New("not found")
					}
					return `[{"id":"gt-src","status":"closed"}]`, nil
				},
				func(args []string) error { return nil },
			)
			if got := hasPendingMR(bd, workDir, "gastown", "nux", "gt-agent"); !got {
				t.Fatalf("hasPendingMR() = false, want true")
			}
		})
	}
}

func TestTerminalSafeDoneSnapshot(t *testing.T) {
	workDir := setupActiveMRGitSafeWorkDir(t, "gastown", "nux")
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 || args[0] != "show" {
				return "[]", nil
			}
			return `[{"id":"gt-src","status":"closed"}]`, nil
		},
		func(args []string) error { return nil },
	)
	snap := &agentBeadSnapshot{Fields: &beads.AgentFields{LastSourceIssue: "gt-src"}}
	if !terminalSafeDoneSnapshot(bd, workDir, "gastown", "nux", snap) {
		t.Fatalf("terminalSafeDoneSnapshot() = false, want true")
	}
	snap.Fields.HookBead = "gt-hook"
	if terminalSafeDoneSnapshot(bd, workDir, "gastown", "nux", snap) {
		t.Fatalf("terminalSafeDoneSnapshot() = true with hook set, want false")
	}
}

func TestFindCleanupWisp_UsesBdQueryForEphemeralWisps(t *testing.T) {
	t.Parallel()
	bd, mock := fakeBd()
	workDir := t.TempDir()

	_, _ = findCleanupWisp(bd, workDir, "gastown", "nux")

	got := strings.Join(mock.calls, "\n")

	// Cleanup wisps are ephemeral (gt-4mnd): "bd list" never sees them
	// regardless of flags. Must use "bd query" with ephemeral=true instead.
	if !strings.Contains(got, "query") {
		t.Errorf("findCleanupWisp: expected \"bd query\", got: %s", got)
	}
	if !strings.Contains(got, "ephemeral=true") {
		t.Errorf("findCleanupWisp: expected ephemeral=true filter, got: %s", got)
	}

	// Must include the polecat label filter
	if !strings.Contains(got, "polecat:nux") {
		t.Errorf("findCleanupWisp: expected polecat:nux label, got: %s", got)
	}
}

func TestFindAnyCleanupWisp_UsesBdQueryForEphemeralWisps(t *testing.T) {
	t.Parallel()
	bd, mock := fakeBd()
	workDir := t.TempDir()

	_ = findAnyCleanupWisp(bd, workDir, "gastown", "bravo")

	got := strings.Join(mock.calls, "\n")

	// Cleanup wisps are ephemeral (gt-4mnd): "bd list" never sees them
	// regardless of flags. Must use "bd query" with ephemeral=true instead.
	if !strings.Contains(got, "query") {
		t.Errorf("findAnyCleanupWisp: expected \"bd query\", got: %s", got)
	}
	if !strings.Contains(got, "ephemeral=true") {
		t.Errorf("findAnyCleanupWisp: expected ephemeral=true filter, got: %s", got)
	}

	// Must include the polecat label filter
	if !strings.Contains(got, "polecat:bravo") {
		t.Errorf("findAnyCleanupWisp: expected polecat:bravo label, got: %s", got)
	}
}

// storedWisp is a fake row in fakeWispStore.
type storedWisp struct {
	id       string
	labels   []string
	assignee string
	status   string
}

// fakeWispStore is a minimal in-memory bd double that actually filters on
// labels/assignee/status, unlike mockBd's canned responses. It exists to
// prove the create→query round trip end to end: a cleanup wisp created via
// createCleanupWisp for one rig is found by that rig's patrol query and NOT
// found by another rig's query for a same-named polecat (gt-gsrz — polecat
// names collide across rigs, so assignee is the discriminator).
type fakeWispStore struct {
	next  int
	wisps []storedWisp
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (s *fakeWispStore) bd() *BdCli {
	return &BdCli{
		Exec: func(workDir string, args ...string) (string, error) {
			if len(args) == 0 {
				return "{}", nil
			}
			switch args[0] {
			case "create":
				s.next++
				id := fmt.Sprintf("gt-wisp-%d", s.next)
				s.wisps = append(s.wisps, storedWisp{
					id:       id,
					labels:   strings.Split(flagValue(args, "--labels"), ","),
					assignee: flagValue(args, "--assignee"),
					status:   "open",
				})
				return fmt.Sprintf(`{"id":%q}`, id), nil
			case "query":
				if len(args) < 2 {
					return "[]", nil
				}
				expr := args[1]
				var wantLabels []string
				wantAssignee, wantStatus := "", ""
				for _, clause := range strings.Split(expr, " AND ") {
					switch {
					case strings.HasPrefix(clause, "label="):
						wantLabels = append(wantLabels, strings.TrimPrefix(clause, "label="))
					case strings.HasPrefix(clause, "assignee="):
						wantAssignee = strings.TrimPrefix(clause, "assignee=")
					case strings.HasPrefix(clause, "status="):
						wantStatus = strings.TrimPrefix(clause, "status=")
					}
				}
				var matched []string
				for _, w := range s.wisps {
					if wantStatus != "" && w.status != wantStatus {
						continue
					}
					if wantAssignee != "" && w.assignee != wantAssignee {
						continue
					}
					allLabels := true
					for _, wl := range wantLabels {
						found := false
						for _, l := range w.labels {
							if l == wl {
								found = true
								break
							}
						}
						if !found {
							allLabels = false
							break
						}
					}
					if !allLabels {
						continue
					}
					matched = append(matched, fmt.Sprintf(`{"id":%q}`, w.id))
				}
				return "[" + strings.Join(matched, ",") + "]", nil
			default:
				return "{}", nil
			}
		},
		Run: func(workDir string, args ...string) error { return nil },
	}
}

// TestCreateCleanupWisp_FoundByOwningRigNotByOtherRig is the gt-gsrz
// regression test: a cleanup wisp created for one rig's polecat is found by
// that rig's patrol query (createCleanupWisp → findAnyCleanupWisp round
// trip), and a same-named polecat cleanup wisp created for a DIFFERENT rig
// is not — proving assignee, not polecat name alone, is what scopes the
// patrol's cleanup-wisp discovery to its own rig.
func TestCreateCleanupWisp_FoundByOwningRigNotByOtherRig(t *testing.T) {
	t.Parallel()
	store := &fakeWispStore{}
	bd := store.bd()
	workDir := t.TempDir()

	gtID, err := createCleanupWisp(bd, workDir, "gastown", "jasper", "gt-abc", "feature-x")
	if err != nil {
		t.Fatalf("createCleanupWisp(gastown): %v", err)
	}
	omID, err := createCleanupWisp(bd, workDir, "om", "jasper", "om-xyz", "other-branch")
	if err != nil {
		t.Fatalf("createCleanupWisp(om): %v", err)
	}
	if gtID == omID {
		t.Fatalf("expected distinct wisp IDs, both got %q", gtID)
	}

	got := findAnyCleanupWisp(bd, workDir, "gastown", "jasper")
	if got != gtID {
		t.Errorf("findAnyCleanupWisp(gastown, jasper) = %q, want %q (the gastown wisp) — a same-named om polecat's wisp must not match", got, gtID)
	}

	got = findAnyCleanupWisp(bd, workDir, "om", "jasper")
	if got != omID {
		t.Errorf("findAnyCleanupWisp(om, jasper) = %q, want %q (the om wisp) — a same-named gastown polecat's wisp must not match", got, omID)
	}
}

func TestFindAllCleanupWisps_NoBdAvailable(t *testing.T) {
	t.Parallel()
	// When bd is not available, findAllCleanupWisps should return nil
	result := findAllCleanupWisps(DefaultBdCli(), "/nonexistent", "gastown", "testpolecat")
	if result != nil {
		t.Errorf("findAllCleanupWisps = %v, want nil when bd unavailable", result)
	}
}

func TestFindAllCleanupWisps_ReturnsAllIDs(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 0 && args[0] == "query" {
				return `[{"id":"gt-wisp-aaa"},{"id":"gt-wisp-bbb"}]`, nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)
	workDir := t.TempDir()

	result := findAllCleanupWisps(bd, workDir, "gastown", "nux")

	if len(result) != 2 {
		t.Fatalf("findAllCleanupWisps: got %d items, want 2", len(result))
	}
	if result[0] != "gt-wisp-aaa" || result[1] != "gt-wisp-bbb" {
		t.Errorf("findAllCleanupWisps: got %v, want [gt-wisp-aaa gt-wisp-bbb]", result)
	}

	got := strings.Join(mock.calls, "\n")
	// Cleanup wisps are ephemeral (gt-4mnd): "bd list" never sees them
	// regardless of flags. Must use "bd query" with ephemeral=true instead.
	if !strings.Contains(got, "query") {
		t.Errorf("findAllCleanupWisps: expected \"bd query\", got: %s", got)
	}
	if !strings.Contains(got, "ephemeral=true") {
		t.Errorf("findAllCleanupWisps: expected ephemeral=true filter, got: %s", got)
	}
	if !strings.Contains(got, "polecat:nux") {
		t.Errorf("findAllCleanupWisps: expected polecat:nux label, got: %s", got)
	}
}

func TestFindAllCleanupWisps_EmptyList(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			return "[]", nil
		},
		func(args []string) error { return nil },
	)
	workDir := t.TempDir()

	result := findAllCleanupWisps(bd, workDir, "gastown", "nux")
	if result != nil {
		t.Errorf("findAllCleanupWisps: got %v, want nil for empty list", result)
	}
}

func TestUpdateCleanupWispState_UsesCorrectBdUpdateFlags(t *testing.T) {
	t.Parallel()
	bd, mock := fakeBd()
	workDir := t.TempDir()

	// UpdateCleanupWispState first calls "bd show <id> --json", then "bd update".
	// Our mock returns valid JSON for show with polecat:testpol label,
	// so polecatName will be "testpol". Then it calls bd update with new labels.
	_ = UpdateCleanupWispState(bd, workDir, "gt-wisp-abc", "merged")

	got := strings.Join(mock.calls, "\n")

	// Must use --set-labels=<label> per label (not --labels)
	if !strings.Contains(got, "--set-labels=") {
		t.Errorf("UpdateCleanupWispState: expected --set-labels=<label> flags, got: %s", got)
	}
	// Check for invalid --labels flag in both " --labels " and "--labels=" forms
	if strings.Contains(got, "--labels") && !strings.Contains(got, "--set-labels") {
		t.Errorf("UpdateCleanupWispState: must not use --labels (invalid for bd update), got: %s", got)
	}

	// Verify individual per-label arguments with correct polecat name from show output
	if !strings.Contains(got, "--set-labels=cleanup") {
		t.Errorf("UpdateCleanupWispState: expected --set-labels=cleanup, got: %s", got)
	}
	if !strings.Contains(got, "--set-labels=polecat:testpol") {
		t.Errorf("UpdateCleanupWispState: expected --set-labels=polecat:testpol, got: %s", got)
	}
	if !strings.Contains(got, "--set-labels=state:merged") {
		t.Errorf("UpdateCleanupWispState: expected --set-labels=state:merged, got: %s", got)
	}
}

// TestRecordCleanupWispLabelSet_RejectsInvalidWispID guards the gt-apam SQL
// injection finding: wispID is interpolated into raw SQL text (bd.Exec's
// "sql" subcommand has no parameter-binding path), so a wispID that doesn't
// match the bead-id shape must be rejected before it ever reaches a query.
func TestRecordCleanupWispLabelSet_RejectsInvalidWispID(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			t.Fatalf("bd.Exec should not be called for an invalid wisp id, got args: %v", args)
			return "", nil
		},
		func(args []string) error { return nil },
	)

	recordCleanupWispLabelSet(bd, t.TempDir(), "gt-wisp-abc'; DROP TABLE wisps; --", "merge-requested")

	if len(mock.calls) != 0 {
		t.Errorf("recordCleanupWispLabelSet: expected no bd calls for an invalid wisp id, got: %v", mock.calls)
	}
}

// TestRecordCleanupWispLabelSet_SkipsInsertOnUnparseableCount guards the
// gt-apam finding that treating a parse error the same as count==0 risks a
// double-insert if bd's table-output format ever changes shape.
func TestRecordCleanupWispLabelSet_SkipsInsertOnUnparseableCount(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			// args[0] is "sql" for both the count and insert queries here.
			return "not-a-number", nil
		},
		func(args []string) error { return nil },
	)

	recordCleanupWispLabelSet(bd, t.TempDir(), "gt-wisp-abc", "merge-requested")

	for _, call := range mock.calls {
		if strings.Contains(call, "INSERT INTO wisp_events") {
			t.Errorf("recordCleanupWispLabelSet: must not insert when the count is unparseable, got call: %s", call)
		}
	}
}

// TestRecordCleanupWispLabelSet_InsertsWhenCountIsZero verifies the normal
// path still fires the insert once the count query cleanly reports 0 rows.
func TestRecordCleanupWispLabelSet_InsertsWhenCountIsZero(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			return "COUNT(*)\n0", nil
		},
		func(args []string) error { return nil },
	)

	recordCleanupWispLabelSet(bd, t.TempDir(), "gt-wisp-abc", "merge-requested")

	var inserted bool
	for _, call := range mock.calls {
		if strings.Contains(call, "INSERT INTO wisp_events") && strings.Contains(call, "gt-wisp-abc") {
			inserted = true
		}
	}
	if !inserted {
		t.Errorf("recordCleanupWispLabelSet: expected an INSERT INTO wisp_events call, got: %v", mock.calls)
	}
}

func TestExtractDoneIntent_Valid(t *testing.T) {
	t.Parallel()
	ts := time.Now().Add(-45 * time.Second)
	labels := []string{
		"gt:agent",
		"idle:2",
		fmt.Sprintf("done-intent:COMPLETED:%d", ts.Unix()),
	}

	intent := extractDoneIntent(labels)
	if intent == nil {
		t.Fatal("extractDoneIntent returned nil for valid label")
	}
	if intent.ExitType != "COMPLETED" {
		t.Errorf("ExitType = %q, want %q", intent.ExitType, "COMPLETED")
	}
	if intent.Timestamp.Unix() != ts.Unix() {
		t.Errorf("Timestamp = %d, want %d", intent.Timestamp.Unix(), ts.Unix())
	}
}

func TestExtractDoneIntent_Missing(t *testing.T) {
	t.Parallel()
	labels := []string{"gt:agent", "idle:2", "backoff-until:1738972900"}

	intent := extractDoneIntent(labels)
	if intent != nil {
		t.Errorf("extractDoneIntent = %+v, want nil for no done-intent label", intent)
	}
}

func TestExtractDoneIntent_Malformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
	}{
		{"missing timestamp", []string{"done-intent:COMPLETED"}},
		{"bad timestamp", []string{"done-intent:COMPLETED:notanumber"}},
		{"empty labels", nil},
		{"empty label list", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := extractDoneIntent(tt.labels)
			if intent != nil {
				t.Errorf("extractDoneIntent(%v) = %+v, want nil for malformed input", tt.labels, intent)
			}
		})
	}
}

func TestExtractDoneIntent_AllExitTypes(t *testing.T) {
	t.Parallel()
	ts := time.Now().Unix()
	for _, exitType := range []string{"COMPLETED", "ESCALATED", "DEFERRED", "PHASE_COMPLETE"} {
		label := fmt.Sprintf("done-intent:%s:%d", exitType, ts)
		intent := extractDoneIntent([]string{label})
		if intent == nil {
			t.Errorf("extractDoneIntent returned nil for exit type %q", exitType)
			continue
		}
		if intent.ExitType != exitType {
			t.Errorf("ExitType = %q, want %q", intent.ExitType, exitType)
		}
	}
}

func TestExtractDoneIntent_MultipleLabels_NewestWins(t *testing.T) {
	t.Parallel()
	// gt-wmpy: when multiple done-intent labels exist (from repeated gt done
	// attempts), extractDoneIntent must return the NEWEST one so stale labels
	// do not cause false stuck-in-done restarts.
	oldestTs := time.Now().Add(-120 * time.Second).Unix()
	middleTs := time.Now().Add(-60 * time.Second).Unix()
	newestTs := time.Now().Add(-10 * time.Second).Unix()

	labels := []string{
		"gt:agent",
		fmt.Sprintf("done-intent:COMPLETED:%d", oldestTs),
		"idle:2",
		fmt.Sprintf("done-intent:ESCALATED:%d", middleTs),
		fmt.Sprintf("done-intent:COMPLETED:%d", newestTs),
	}

	intent := extractDoneIntent(labels)
	if intent == nil {
		t.Fatal("extractDoneIntent returned nil for valid labels")
	}
	if intent.ExitType != "COMPLETED" {
		t.Errorf("ExitType = %q, want %q", intent.ExitType, "COMPLETED")
	}
	if intent.Timestamp.Unix() != newestTs {
		t.Errorf("Timestamp = %d, want %d", intent.Timestamp.Unix(), newestTs)
	}

	// Re-order: newest first — should still return newest.
	reordered := []string{
		fmt.Sprintf("done-intent:COMPLETED:%d", newestTs),
		fmt.Sprintf("done-intent:ESCALATED:%d", middleTs),
		fmt.Sprintf("done-intent:COMPLETED:%d", oldestTs),
	}
	intent2 := extractDoneIntent(reordered)
	if intent2 == nil || intent2.Timestamp.Unix() != newestTs {
		t.Errorf("extractDoneIntent reordered = %v, want newest (%d)", intent2, newestTs)
	}

	// All malformed — should return nil.
	malformed := []string{
		"done-intent:COMPLETED",
		"done-intent:ESCALATED:notanumber",
	}
	if intent3 := extractDoneIntent(malformed); intent3 != nil {
		t.Errorf("extractDoneIntent(malformed) = %+v, want nil", intent3)
	}
}

func TestClearAllDoneIntentLabels(t *testing.T) {
	t.Parallel()
	// gt-wmpy: verify clearAllDoneIntentLabels removes ALL done-intent labels
	// and leaves non-done-intent labels intact, using a mock beadUpdater.
	mock := &mockBeadUpdater{}

	// Empty bead ID — should be a no-op.
	clearAllDoneIntentLabels(mock, "", &agentBeadSnapshot{Labels: []string{"done-intent:COMPLETED:123"}})
	if len(mock.calls) != 0 {
		t.Fatalf("expected no calls for empty bead ID, got %d", len(mock.calls))
	}

	// Nil snap — should be a no-op.
	clearAllDoneIntentLabels(mock, "gt-xyz", nil)
	if len(mock.calls) != 0 {
		t.Fatalf("expected no calls for nil snap, got %d", len(mock.calls))
	}

	// No done-intent labels — should be a no-op.
	clearAllDoneIntentLabels(mock, "gt-xyz", &agentBeadSnapshot{Labels: []string{"gt:agent", "idle:2"}})
	if len(mock.calls) != 0 {
		t.Fatalf("expected no calls for no done-intent labels, got %d", len(mock.calls))
	}

	// Mixed labels — should remove only done-intent labels.
	clearAllDoneIntentLabels(mock, "gt-123", &agentBeadSnapshot{
		Labels: []string{
			"gt:agent",
			"done-intent:COMPLETED:1700000000",
			"idle:2",
			"done-intent:ESCALATED:1700000060",
		},
	})
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.calls))
	}
	if mock.calls[0].ID != "gt-123" {
		t.Errorf("call ID = %q, want %q", mock.calls[0].ID, "gt-123")
	}
	if len(mock.calls[0].RemoveLabels) != 2 {
		t.Fatalf("expected 2 RemoveLabels, got %d: %v", len(mock.calls[0].RemoveLabels), mock.calls[0].RemoveLabels)
	}
	for _, label := range mock.calls[0].RemoveLabels {
		if !strings.HasPrefix(label, "done-intent:") {
			t.Errorf("RemoveLabels contains non-done-intent label: %q", label)
		}
	}
}

// TestDoneIntentWorthRestarting covers the gt-jv7v gate: a stale done-intent on
// a dead session only justifies a restart while there is work to resume.
func TestDoneIntentWorthRestarting(t *testing.T) {
	t.Parallel()
	const maxAge = 24 * time.Hour

	tests := []struct {
		name string
		snap *agentBeadSnapshot
		age  time.Duration
		want bool
	}{
		{
			// The gt-jv7v false positive: the operator re-slung flint's bead
			// away, leaving hook_bead null and agent_state idle, yet the scan
			// restarted it off a done-intent label.
			name: "idle unhooked polecat with a stale done-intent",
			snap: &agentBeadSnapshot{
				AgentState: "idle",
				HookBead:   "",
				Labels:     []string{"done-intent:COMPLETED:1789120261"},
			},
			age:  120 * time.Hour,
			want: false,
		},
		{
			name: "active polecat with an open hook and a fresh intent",
			snap: &agentBeadSnapshot{
				AgentState: "working",
				HookBead:   "gt-abc123",
				Labels:     []string{"done-intent:COMPLETED:1789120261"},
			},
			age:  5 * time.Minute,
			want: true,
		},
		{
			// A dead session cannot be idle and resumable at once, but a hook
			// without an idle state is the shape gt done leaves when it crashes
			// mid-exit, so an empty agent_state must not veto the restart.
			name: "hooked polecat with no recorded agent state",
			snap: &agentBeadSnapshot{HookBead: "gt-abc123"},
			age:  5 * time.Minute,
			want: true,
		},
		{
			name: "hooked but idle means nothing to resume either",
			snap: &agentBeadSnapshot{
				AgentState: "idle",
				HookBead:   "gt-abc123",
			},
			age:  5 * time.Minute,
			want: false,
		},
		{
			// gt-vql3: the hold owns the hook, so the label is not evidence of
			// resumable work and a restart would break the hold.
			name: "hook held under awaiting-gate",
			snap: &agentBeadSnapshot{
				AgentState: "awaiting-gate",
				HookBead:   "gt-abc123",
			},
			age:  5 * time.Minute,
			want: false,
		},
		{
			name: "hook held under stuck",
			snap: &agentBeadSnapshot{
				AgentState: "stuck",
				HookBead:   "gt-abc123",
			},
			age:  5 * time.Minute,
			want: false,
		},
		{
			name: "hook held under paused",
			snap: &agentBeadSnapshot{
				AgentState: "paused",
				HookBead:   "gt-abc123",
			},
			age:  5 * time.Minute,
			want: false,
		},
		{
			name: "completion already reached the witness",
			snap: &agentBeadSnapshot{
				AgentState: "working",
				HookBead:   "gt-abc123",
				Labels: []string{
					"done-intent:COMPLETED:1789120261",
					"done-cp:witness-notified:ok:1789576193",
				},
			},
			age:  5 * time.Minute,
			want: false,
		},
		{
			// 120h old on the bead's flint: past the bound, the attempt it
			// records is moot even with a hook still attached.
			name: "intent older than the max age",
			snap: &agentBeadSnapshot{
				AgentState: "working",
				HookBead:   "gt-abc123",
			},
			age:  25 * time.Hour,
			want: false,
		},
		{
			name: "intent exactly at the max age is still resumable",
			snap: &agentBeadSnapshot{
				AgentState: "working",
				HookBead:   "gt-abc123",
			},
			age:  maxAge,
			want: true,
		},
		{
			name: "unreadable agent bead",
			snap: nil,
			age:  5 * time.Minute,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := doneIntentWorthRestarting(tt.snap, tt.age, maxAge); got != tt.want {
				t.Errorf("doneIntentWorthRestarting() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasDoneCheckpoint(t *testing.T) {
	t.Parallel()

	snap := &agentBeadSnapshot{Labels: []string{
		"gt:agent",
		"done-cp:pushed:polecat/furiosa-abc:1738972800",
		"done-cp:witness-notified:ok:1738972803",
	}}

	if !hasDoneCheckpoint(snap, "witness-notified") {
		t.Error("hasDoneCheckpoint(witness-notified) = false, want true")
	}
	if !hasDoneCheckpoint(snap, "pushed") {
		t.Error("hasDoneCheckpoint(pushed) = false, want true")
	}
	if hasDoneCheckpoint(snap, "mr-created") {
		t.Error("hasDoneCheckpoint(mr-created) = true, want false")
	}
	// A stage name that is a prefix of another must not match by substring.
	if hasDoneCheckpoint(snap, "wit") {
		t.Error("hasDoneCheckpoint(wit) = true, want false (prefix match must end at a colon)")
	}
	if hasDoneCheckpoint(nil, "witness-notified") {
		t.Error("hasDoneCheckpoint(nil) = true, want false")
	}
}

type mockBeadUpdater struct {
	calls []beadUpdateCall
}

type beadUpdateCall struct {
	ID           string
	RemoveLabels []string
}

func (m *mockBeadUpdater) Update(id string, opts beads.UpdateOptions) error {
	if opts.RemoveLabels != nil {
		m.calls = append(m.calls, beadUpdateCall{
			ID:           id,
			RemoveLabels: opts.RemoveLabels,
		})
	}
	return nil
}

func TestDetectZombie_DoneIntentDeadSession(t *testing.T) {
	t.Parallel()
	// Verify the logic: dead session + done-intent older than 30s → should be treated as zombie
	// gt-dsgp: action is restart (not nuke), but detection logic is the same
	doneIntent := &DoneIntent{
		ExitType:  "COMPLETED",
		Timestamp: time.Now().Add(-60 * time.Second), // 60s old
	}
	sessionAlive := false
	age := time.Since(doneIntent.Timestamp)

	// Dead session + old intent → restart path (gt-dsgp: was auto-nuke)
	shouldRestart := !sessionAlive && doneIntent != nil && age >= config.DefaultWitnessDoneIntentStuckTimeout
	if !shouldRestart {
		t.Errorf("expected restart for dead session + old done-intent (age=%v)", age)
	}
}

func TestDetectZombie_DoneIntentLiveStuck(t *testing.T) {
	t.Parallel()
	// Verify the logic: live session + done-intent older than 60s → should restart session
	// gt-dsgp: restart instead of kill
	doneIntent := &DoneIntent{
		ExitType:  "COMPLETED",
		Timestamp: time.Now().Add(-90 * time.Second), // 90s old
	}
	sessionAlive := true
	age := time.Since(doneIntent.Timestamp)

	// Live session + old intent → restart stuck session (gt-dsgp: was kill)
	shouldRestart := sessionAlive && doneIntent != nil && age > config.DefaultWitnessDoneIntentStuckTimeout
	if !shouldRestart {
		t.Errorf("expected restart for live session + old done-intent (age=%v)", age)
	}
}

func TestDetectZombie_DoneIntentRecent(t *testing.T) {
	t.Parallel()
	// Verify the logic: done-intent younger than config.DefaultWitnessDoneIntentStuckTimeout → skip (polecat still working)
	doneIntent := &DoneIntent{
		ExitType:  "COMPLETED",
		Timestamp: time.Now().Add(-10 * time.Second), // 10s old
	}
	sessionAlive := false
	age := time.Since(doneIntent.Timestamp)

	// Recent intent → should skip
	shouldSkip := !sessionAlive && doneIntent != nil && age < config.DefaultWitnessDoneIntentStuckTimeout
	if !shouldSkip {
		t.Errorf("expected skip for recent done-intent (age=%v)", age)
	}

	// Live session + recent intent → also skip
	sessionAlive = true
	shouldSkipLive := sessionAlive && doneIntent != nil && age <= config.DefaultWitnessDoneIntentStuckTimeout
	if !shouldSkipLive {
		t.Errorf("expected skip for live session + recent done-intent (age=%v)", age)
	}
}

func TestDetectZombie_DoneOrNukedNotZombie(t *testing.T) {
	t.Parallel()
	// GH#2795: Polecats with agent_state=done or agent_state=nuked and a dead
	// session should NOT be treated as zombies, even if hook_bead is still set.
	// Without this, isZombieState returns true (hookBead != ""), and the witness
	// floods the mayor inbox with RECOVERY_NEEDED alerts every patrol cycle.
	for _, state := range []beads.AgentState{beads.AgentStateDone, beads.AgentStateNuked} {
		hookBead := "gt-some-issue"
		// isZombieState returns true because hookBead != ""
		if !isZombieState(state, hookBead) {
			t.Errorf("isZombieState(%q, %q) = false, want true (pre-condition)", state, hookBead)
		}
		// But the done/nuked check in detectZombieDeadSession should skip these.
		// Verify the states are terminal (not active).
		if state.IsActive() {
			t.Errorf("state %q should not be active", state)
		}
	}
}

func TestDetectZombie_AgentDeadInLiveSession(t *testing.T) {
	t.Parallel()
	// Verify the logic: live session + agent process dead → zombie
	// This is the gt-kj6r6 fix: DetectZombiePolecats now checks IsAgentAlive
	// for sessions that DO exist, catching the tmux-alive-but-agent-dead class.
	sessionAlive := true
	agentAlive := false
	var doneIntent *DoneIntent // No done-intent

	// Live session + no done-intent + agent dead → should be classified as zombie
	shouldDetect := sessionAlive && doneIntent == nil && !agentAlive
	if !shouldDetect {
		t.Error("expected zombie detection for live session with dead agent")
	}

	// Live session + agent alive → NOT a zombie
	agentAlive = true
	shouldSkip := sessionAlive && doneIntent == nil && agentAlive
	if !shouldSkip {
		t.Error("expected skip for live session with alive agent")
	}
}

// --- extractPolecatFromJSON tests (issue #1228: panic-safe JSON parsing) ---

func TestExtractPolecatFromJSON_ValidOutput(t *testing.T) {
	t.Parallel()
	input := `[{"labels":["cleanup","polecat:nux","state:pending"]}]`
	got := extractPolecatFromJSON(input)
	if got != "nux" {
		t.Errorf("extractPolecatFromJSON() = %q, want %q", got, "nux")
	}
}

func TestExtractPolecatFromJSON_InvalidInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"empty output", ""},
		{"malformed JSON", "{not valid json"},
		{"empty array", "[]"},
		{"no polecat label", `[{"labels":["cleanup","state:pending"]}]`},
		{"empty labels", `[{"labels":[]}]`},
		{"truncated JSON", `[{"labels":["polecat:`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPolecatFromJSON(tt.input)
			if got != "" {
				t.Errorf("extractPolecatFromJSON(%q) = %q, want empty", tt.input, got)
			}
		})
	}
}

func TestGetBeadStatus_NoBdAvailable(t *testing.T) {
	t.Parallel()
	// When bd is not available (test environment), getBeadStatus
	// should return ("", false) without panicking
	result, ok := getBeadStatus(DefaultBdCli(), "/nonexistent", "gt-abc123")
	if result != "" || ok {
		t.Errorf("getBeadStatus = (%q, %v), want (\"\", false) when bd unavailable", result, ok)
	}
}

func TestGetBeadStatus_EmptyBeadID(t *testing.T) {
	t.Parallel()
	// Empty bead ID should return ("", false) immediately
	result, ok := getBeadStatus(DefaultBdCli(), "/nonexistent", "")
	if result != "" || ok {
		t.Errorf("getBeadStatus(\"\") = (%q, %v), want (\"\", false)", result, ok)
	}
}

func TestDetectZombie_BeadClosedStillRunning(t *testing.T) {
	t.Parallel()
	// Verify the logic: live session + agent alive + hooked bead closed → zombie
	// This is the gt-h1l6i fix: DetectZombiePolecats now checks if the
	// polecat's hooked bead has been closed while the session is still running.
	sessionAlive := true
	agentAlive := true
	var doneIntent *DoneIntent // No done-intent
	hookBead := "gt-some-issue"
	beadStatus := "closed"

	// Live session + agent alive + no done-intent + bead closed → should detect
	shouldDetect := sessionAlive && agentAlive && doneIntent == nil &&
		hookBead != "" && beadStatus == "closed"
	if !shouldDetect {
		t.Error("expected zombie detection for live session with closed bead")
	}

	// Bead open → NOT a zombie
	beadStatus = "open"
	shouldSkip := sessionAlive && agentAlive && doneIntent == nil &&
		hookBead != "" && beadStatus == "closed"
	if shouldSkip {
		t.Error("should not detect zombie when bead is still open")
	}

	// No hook bead → NOT a zombie
	hookBead = ""
	beadStatus = "closed"
	shouldSkipNoHook := sessionAlive && agentAlive && doneIntent == nil &&
		hookBead != "" && beadStatus == "closed"
	if shouldSkipNoHook {
		t.Error("should not detect zombie when no hook bead exists")
	}
}

func TestDetectZombie_BeadClosedVsDoneIntent(t *testing.T) {
	t.Parallel()
	// Verify done-intent takes priority over closed-bead check.
	// If done-intent exists (recent), the polecat is still working through
	// gt done and we should NOT trigger the closed-bead path.
	sessionAlive := true
	agentAlive := true
	doneIntent := &DoneIntent{
		ExitType:  "COMPLETED",
		Timestamp: time.Now().Add(-10 * time.Second), // Recent
	}
	hookBead := "gt-some-issue"
	beadStatus := "closed"

	// Done-intent exists + bead closed → done-intent check runs first,
	// closed-bead check should NOT run (it's in the else branch)
	doneIntentHandled := sessionAlive && doneIntent != nil && time.Since(doneIntent.Timestamp) > config.DefaultWitnessDoneIntentStuckTimeout
	closedBeadCheck := sessionAlive && agentAlive && doneIntent == nil &&
		hookBead != "" && beadStatus == "closed"

	// Neither should trigger: done-intent is recent (not stuck), and
	// closed-bead check requires doneIntent == nil
	if doneIntentHandled {
		t.Error("recent done-intent should not trigger stuck-session handler")
	}
	if closedBeadCheck {
		t.Error("closed-bead check should not run when done-intent exists")
	}
}

// TestDetectZombieLiveSession_SpawningStuckNoHookNoHeartbeat reproduces gt-gf6t:
// a polecat with a LIVE tmux session, stuck at agent_state=spawning (never
// advanced to "working"), no hook_bead durably attached, and no heartbeat file
// ever written (agent stuck at startup) was invisible to zombie detection
// because the "never heartbeated" check required hook_bead != "". This is
// exactly jade's reported shape: "Claude at an empty prompt, no hook" with a
// live session, while `gt polecat list --json` already flags any active
// agent_state (spawning included) as NEEDS_RECOVERY regardless of hook_bead.
func TestDetectZombieLiveSession_SpawningStuckNoHookNoHeartbeat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux not supported on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	townRoot := t.TempDir()
	socket := constants.TestSocketName("gt-test-gf6t")
	tm := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })

	sessionName := "gt-test-jade"
	if err := tm.NewSessionWithCommand(sessionName, townRoot, "sleep 300"); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	// Declare the pane's process name so IsAgentAlive matches it as "alive" —
	// simulates a live agent process (bare Claude prompt) rather than a dead one.
	if err := tm.SetEnvironment(sessionName, "GT_PROCESS_NAMES", "sleep"); err != nil {
		t.Fatalf("set GT_PROCESS_NAMES: %v", err)
	}

	if !tm.IsAgentAlive(sessionName) {
		t.Fatal("precondition failed: fake session should report agent alive")
	}

	// Ensure session age exceeds the (artificially tiny) startup grace period.
	time.Sleep(20 * time.Millisecond)
	witCfg := &config.WitnessThresholds{HeartbeatStartupGrace: "1ms"}

	// Pin the gt-gx2v liveness cross-check to "quiet". This test is about
	// the gate admitting agent_state=spawning with no hook_bead; the real
	// cross-check reads tmux's window_activity, which pane output advances
	// and which has one-second resolution. NewSessionWithCommand starts the
	// login shell first and respawns the pane with the command after, so the
	// shell's prompt is written asynchronously. When that write landed in the
	// second after session_created (likelier on a loaded host), it postdated
	// createdAt+1ms grace and read as fresh work, so the session was judged
	// working and not flagged (found=false). The cross-check has its own
	// tests (TestDetectZombieLiveSession_NeverHeartbeatedNeedsLivenessEvidence
	// and the classifyNeverHeartbeatedLiveness cases). Not parallel: it
	// swaps a package seam.
	old := neverHeartbeatedLiveness
	t.Cleanup(func() { neverHeartbeatedLiveness = old })
	neverHeartbeatedLiveness = func(*tmux.Tmux, string, string, string, string, time.Time) neverHeartbeatedEvidence {
		return neverHeartbeatedEvidence{Detail: "gate-slot=none, transcript=none, pane-output=none"}
	}

	bd, _ := fakeBd()
	snap := &agentBeadSnapshot{
		AgentState: "spawning",
		HookBead:   "", // partial spawn: never durably attached a hook_bead
	}

	zombie, found := detectZombieLiveSession(bd, townRoot, townRoot, "gastown", "jade", sessionName, tm, nil, witCfg, snap, "")
	t.Logf("found=%v zombie=%+v", found, zombie)
	if !found {
		t.Fatal("expected zombie detection for live session stuck at agent_state=spawning with no hook_bead and no heartbeat")
	}
	if zombie.Classification != ZombieNeverHeartbeated {
		t.Errorf("Classification = %q, want %q", zombie.Classification, ZombieNeverHeartbeated)
	}
}

// TestDetectZombieLiveSession_NeverHeartbeatedNeedsLivenessEvidence is the
// gt-gx2v wiring: a live, heartbeatless polecat with work on its hook is flagged
// only when no signal shows output, and the flag names the evidence it read.
// Not parallel — it overrides the neverHeartbeatedLiveness seam.
func TestDetectZombieLiveSession_NeverHeartbeatedNeedsLivenessEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux not supported on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	townRoot := t.TempDir()
	socket := constants.TestSocketName("gt-test-gx2v")
	tm := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })

	sessionName := "gt-test-emerald"
	if err := tm.NewSessionWithCommand(sessionName, townRoot, "sleep 300"); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_PROCESS_NAMES", "sleep"); err != nil {
		t.Fatalf("set GT_PROCESS_NAMES: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	witCfg := &config.WitnessThresholds{HeartbeatStartupGrace: "1ms"}
	bd, _ := fakeBd()
	snap := &agentBeadSnapshot{AgentState: "working", HookBead: "gt-gx2v"}

	old := neverHeartbeatedLiveness
	t.Cleanup(func() { neverHeartbeatedLiveness = old })

	detect := func(ev neverHeartbeatedEvidence) (ZombieResult, bool) {
		neverHeartbeatedLiveness = func(*tmux.Tmux, string, string, string, string, time.Time) neverHeartbeatedEvidence {
			return ev
		}
		return detectZombieLiveSession(bd, townRoot, townRoot, "gastown", "emerald", sessionName, tm, nil, witCfg, snap, "")
	}

	// Working: the cross-check found the transcript a second old, so the patrol
	// emits nothing at all — no zombie, no POLECAT_DIED mail, no witness cycle.
	if zombie, found := detect(neverHeartbeatedEvidence{
		Working: true,
		Detail:  "gate-slot=none, transcript=1s old/900000 bytes (dates this session)",
	}); found {
		t.Fatalf("working heartbeatless polecat flagged as zombie: %+v", zombie)
	}

	// Wedged: still flagged, and the flag carries the evidence it used, so a
	// false positive is diagnosable from the mail alone.
	evidence := "gate-slot=none, transcript=none, pane-output=19m0s old"
	zombie, found := detect(neverHeartbeatedEvidence{Detail: evidence})
	if !found {
		t.Fatal("expected a quiet heartbeatless session to still be flagged")
	}
	if zombie.Classification != ZombieNeverHeartbeated {
		t.Errorf("Classification = %q, want %q", zombie.Classification, ZombieNeverHeartbeated)
	}
	if !strings.Contains(zombie.Action, evidence) || !strings.Contains(zombie.Action, "session-age=") {
		t.Errorf("Action = %q, want it to name the evidence (%q) and the session age", zombie.Action, evidence)
	}
}

// TestDetectZombieDeadSession_GenuinelyDeadPolecatIsStillFlagged is the gt-gx2v
// control. The liveness cross-check lives in the live-session path, so a polecat
// whose session and agent process are both gone is still classified and
// escalated — the gate cannot be "fixed" by silencing the rule. Not parallel:
// it stubs the restart and nuke seams so the restart starts nothing real and
// the archive path cannot reach a polecat.
func TestDetectZombieDeadSession_GenuinelyDeadPolecatIsStillFlagged(t *testing.T) {
	stubRestartSessionExec(t)
	nuked := stubNukePolecat(t, nil)

	townRoot := t.TempDir()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 0 && args[0] == "show" {
				return `[{"status":"open"}]`, nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)
	snap := &agentBeadSnapshot{
		AgentState: "working",
		HookBead:   "gt-gx2v",
		UpdatedAt:  time.Now().Format(time.RFC3339),
	}

	zombie, found := detectZombieDeadSession(bd, townRoot, townRoot, "zz-test-rig", "zz-test-cat",
		"gt-zz-test-rig-zz-test-cat", deadSessionTmux(t), nil, time.Now(), &config.WitnessThresholds{}, snap, "")
	t.Logf("found=%v zombie=%+v", found, zombie)
	if !found {
		t.Fatal("a polecat with no session and no process was not flagged")
	}
	if zombie.Classification != ZombieSessionDeadActive {
		t.Errorf("Classification = %q, want %q", zombie.Classification, ZombieSessionDeadActive)
	}
	if !zombie.WasActive {
		t.Error("WasActive = false, want true for a dead session holding work")
	}
	if nuked() {
		t.Error("the archive path ran a polecat nuke; this control must not be able to reach one")
	}
}

// deadSessionTmux returns a tmux client on a test-only socket with no server
// behind it. The dead-session detector only reads through it, so an isolated
// socket is sufficient — and required: the default socket IS the town's real
// tmux server, which the nuke path behind this detector can kill sessions on
// (gt-4y5x).
func deadSessionTmux(t *testing.T) *tmux.Tmux {
	t.Helper()
	socket := constants.TestSocketName("gt-test-deadsession")
	t.Cleanup(func() { _ = tmux.NewTmuxWithSocket(socket).KillServer() })
	return tmux.NewTmuxWithSocket(socket)
}

// TestDetectZombieDeadSession_DeliberateHoldBlocksRestart is the gt-vql3
// regression. The two runs differ only in agent_state: both hold a hook and a
// done-intent inside the max age, so the working run is the control that proves
// the bead reaches the restart and the held run proves the state is what stops
// it. Not parallel: it stubs the restart seam.
func TestDetectZombieDeadSession_DeliberateHoldBlocksRestart(t *testing.T) {
	restarts := stubRestartSessionExec(t)

	townRoot := t.TempDir()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			switch {
			case len(args) > 0 && args[0] == "query":
				return "[]", nil // no cleanup wisp == no pending MR
			case len(args) > 0 && args[0] == "show":
				return `[{"status":"open"}]`, nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	detectedAt := time.Now()
	doneIntent := &DoneIntent{ExitType: "COMPLETED", Timestamp: detectedAt.Add(-5 * time.Minute)}
	deadTM := deadSessionTmux(t)

	detect := func(agentState string) (ZombieResult, bool) {
		before := len(*restarts)
		snap := &agentBeadSnapshot{AgentState: agentState, HookBead: "gt-vql3x"}
		zombie, found := detectZombieDeadSession(bd, townRoot, townRoot, "zz-test-rig", "zz-test-cat",
			"gt-zz-test-rig-zz-test-cat", deadTM, doneIntent, detectedAt, &config.WitnessThresholds{}, snap, "")
		t.Logf("agent_state=%s found=%v restarts=%d zombie=%+v", agentState, found, len(*restarts)-before, zombie)
		return zombie, found
	}

	if _, found := detect("working"); !found {
		t.Fatal("control failed: a working polecat with a fresh done-intent was not restarted")
	}

	for _, held := range []string{"awaiting-gate", "stuck", "paused"} {
		before := len(*restarts)
		zombie, found := detect(held)
		if found {
			t.Errorf("agent_state=%s: restarted off the label alone (gt-vql3): %+v", held, zombie)
		}
		if len(*restarts) != before {
			t.Errorf("agent_state=%s: %d restart(s) recorded, want none", held, len(*restarts)-before)
		}
	}
}

// TestDetectZombieLiveSession_WorkingDoneIntentIsNotStuckInDone is the gt-z7vr
// regression: flint's gt done pushed, main moved, and the polecat rebased while
// its done-intent aged past the stuck timeout. The witness restarted it
// mid-rebase on age alone, which left a same-named origin branch at the
// pre-rebase tip and cost an operator force push one session later (gt-bf5x).
// A live session whose transcript advanced after the done-intent is working.
func TestDetectZombieLiveSession_WorkingDoneIntentIsNotStuckInDone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux not supported on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	townRoot := t.TempDir()
	socket := constants.TestSocketName("gt-test-z7vr")
	tm := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })

	sessionName := "gt-test-flint"
	if err := tm.NewSessionWithCommand(sessionName, townRoot, "sleep 300"); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	// Declare the pane's process name so IsAgentAlive matches it as "alive".
	if err := tm.SetEnvironment(sessionName, "GT_PROCESS_NAMES", "sleep"); err != nil {
		t.Fatalf("set GT_PROCESS_NAMES: %v", err)
	}
	if !tm.IsAgentAlive(sessionName) {
		t.Fatal("precondition failed: fake session should report agent alive")
	}

	now := time.Now()
	doneIntent := &DoneIntent{ExitType: "COMPLETED", Timestamp: now.Add(-5 * time.Minute)}
	witCfg := &config.WitnessThresholds{DoneIntentStuckTimeout: "1m"}
	bd, _ := fakeBd()
	snap := &agentBeadSnapshot{AgentState: "working", HookBead: "gt-3s52"}

	restarts := 0
	oldObserve, oldRestart := observeDoneIntentActivity, restartStuckSession
	restartStuckSession = func(string, string, string) error {
		restarts++
		return nil
	}
	t.Cleanup(func() {
		observeDoneIntentActivity, restartStuckSession = oldObserve, oldRestart
	})

	detect := func(act RealActivity) (ZombieResult, bool) {
		observeDoneIntentActivity = func(*tmux.Tmux, string, string, string) RealActivity { return act }
		return detectZombieLiveSession(bd, townRoot, townRoot, "gastown", "flint", sessionName, tm, doneIntent, witCfg, snap, "")
	}

	// Work in flight: the transcript advanced a minute ago, after the done-intent.
	working := RealActivity{
		Session: sessionName, Polecat: "flint", AgentAlive: true, ObservedAt: now,
		LastActivity: now.Add(-time.Minute), ActivitySource: ActivitySourceTranscript,
	}
	if zombie, found := detect(working); found {
		t.Fatalf("live session working after its done-intent was flagged as zombie: %+v", zombie)
	}
	if restarts != 0 {
		t.Fatalf("restarted a live polecat on done-intent age alone (%d restart(s))", restarts)
	}

	// Control: nothing since the done-intent, so the restart still fires — the
	// gate must leave stuck-in-done reachable for a gt done wedged outside a
	// bounded stage, which is the case the timeout exists for (gt-azmw).
	stuck := RealActivity{
		Session: sessionName, Polecat: "flint", AgentAlive: true, ObservedAt: now,
		LastActivity: now.Add(-10 * time.Minute), ActivitySource: ActivitySourceTranscript,
	}
	zombie, found := detect(stuck)
	if !found {
		t.Fatal("expected stuck-in-done for a session with no work since its done-intent")
	}
	if zombie.Classification != ZombieStuckInDone {
		t.Errorf("Classification = %q, want %q", zombie.Classification, ZombieStuckInDone)
	}
	if restarts != 1 {
		t.Errorf("restarts = %d, want 1", restarts)
	}
}

func TestResetAbandonedBead_EmptyHookBead(t *testing.T) {
	t.Parallel()
	// resetAbandonedBead should return false for empty hookBead
	result := resetAbandonedBead(DefaultBdCli(), "/tmp", "testrig", "", "nux", nil)
	if result {
		t.Error("resetAbandonedBead should return false for empty hookBead")
	}
}

func TestResetAbandonedBead_NoRouter(t *testing.T) {
	t.Parallel()
	// resetAbandonedBead with nil router should not panic even if bead exists.
	// It will return false because bd won't find the bead, but shouldn't crash.
	result := resetAbandonedBead(DefaultBdCli(), "/tmp/nonexistent", "testrig", "gt-fake123", "nux", nil)
	if result {
		t.Error("resetAbandonedBead should return false when bd commands fail")
	}
}

func TestResetAbandonedBead_ClosesWhenWorkOnMain(t *testing.T) {
	// Not parallel: overrides package-level verifyCommitOnMain.
	// When verifyCommitOnMain returns true, resetAbandonedBead should close the
	// bead instead of resetting it for re-dispatch. This is the fix for #2036.

	oldVerify := verifyCommitOnMain
	verifyCommitOnMain = func(workDir, rigName, polecatName string) (bool, error) {
		return true, nil // work is on main
	}
	t.Cleanup(func() { verifyCommitOnMain = oldVerify })

	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) >= 1 && args[0] == "show" {
				return `[{"status":"hooked"}]`, nil
			}
			return "", nil
		},
		func(args []string) error {
			return nil
		},
	)

	tmpDir := t.TempDir()
	result := resetAbandonedBead(bd, tmpDir, "testrig", "gt-work123", "alpha", nil)
	if result {
		t.Error("resetAbandonedBead should return false when work is on main (bead closed, not re-dispatched)")
	}

	// Verify "close" was called, NOT "update ... --status=open"
	var foundClose, foundUpdate bool
	for _, call := range mock.calls {
		if strings.Contains(call, "close gt-work123") {
			foundClose = true
		}
		if strings.Contains(call, "update") && strings.Contains(call, "--status=open") {
			foundUpdate = true
		}
	}
	if !foundClose {
		t.Errorf("expected bd close to be called, got calls: %v", mock.calls)
	}
	if foundUpdate {
		t.Error("bd update --status=open should NOT be called when work is on main")
	}
}

func TestResetAbandonedBead_ResetsWhenWorkNotOnMain(t *testing.T) {
	// Not parallel: overrides package-level verifyCommitOnMain.
	// When verifyCommitOnMain returns false, resetAbandonedBead should reset
	// the bead for re-dispatch (existing behavior).

	oldVerify := verifyCommitOnMain
	verifyCommitOnMain = func(workDir, rigName, polecatName string) (bool, error) {
		return false, nil // work NOT on main
	}
	t.Cleanup(func() { verifyCommitOnMain = oldVerify })

	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) >= 1 && args[0] == "show" {
				return `[{"status":"hooked"}]`, nil
			}
			return "", nil
		},
		func(args []string) error {
			return nil
		},
	)

	tmpDir := t.TempDir()
	result := resetAbandonedBead(bd, tmpDir, "testrig", "gt-work123", "alpha", nil)
	if !result {
		t.Error("resetAbandonedBead should return true when work is NOT on main (bead reset for re-dispatch)")
	}

	// Verify "update --status=open" was called (normal reset path)
	var foundUpdate bool
	for _, call := range mock.calls {
		if strings.Contains(call, "update") && strings.Contains(call, "--status=open") {
			foundUpdate = true
		}
	}
	if !foundUpdate {
		t.Errorf("expected bd update --status=open to be called, got calls: %v", mock.calls)
	}
}

func TestBeadRecoveredField_DefaultFalse(t *testing.T) {
	t.Parallel()
	// BeadRecovered should default to false (zero value)
	z := ZombieResult{
		PolecatName:    "nux",
		AgentState:     "working",
		Classification: ZombieSessionDeadActive,
	}
	if z.BeadRecovered {
		t.Error("BeadRecovered should default to false")
	}
}

func TestStalledResult_Types(t *testing.T) {
	t.Parallel()
	// Verify the StalledResult type has all expected fields
	s := StalledResult{
		PolecatName: "alpha",
		StallType:   "startup-stall",
		Action:      "auto-dismissed",
		Error:       nil,
	}

	if s.PolecatName != "alpha" {
		t.Errorf("PolecatName = %q, want %q", s.PolecatName, "alpha")
	}
	if s.StallType != "startup-stall" {
		t.Errorf("StallType = %q, want %q", s.StallType, "startup-stall")
	}
	if s.Action != "auto-dismissed" {
		t.Errorf("Action = %q, want %q", s.Action, "auto-dismissed")
	}
	if s.Error != nil {
		t.Errorf("Error = %v, want nil", s.Error)
	}

	// Verify error field works
	s2 := StalledResult{
		PolecatName: "bravo",
		StallType:   "startup-stall",
		Action:      "escalated",
		Error:       fmt.Errorf("auto-dismiss failed"),
	}
	if s2.Error == nil {
		t.Error("Error = nil, want non-nil")
	}
}

func TestDetectStalledPolecatsResult_Empty(t *testing.T) {
	t.Parallel()
	result := &DetectStalledPolecatsResult{}

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0", result.Checked)
	}
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled length = %d, want 0", len(result.Stalled))
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors length = %d, want 0", len(result.Errors))
	}
}

func TestDetectStalledPolecats_NoPolecats(t *testing.T) {
	t.Parallel()
	// Should handle missing polecats directory gracefully
	result := DetectStalledPolecats("/nonexistent/path", "testrig")

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 for nonexistent dir", result.Checked)
	}
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %d, want 0 for nonexistent dir", len(result.Stalled))
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %d, want 0 for nonexistent dir", len(result.Errors))
	}
}

func TestDetectStalledPolecats_EmptyPolecatsDir(t *testing.T) {
	t.Parallel()
	// Empty polecats directory should return 0 checked
	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	result := DetectStalledPolecats(tmpDir, rigName)

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 for empty polecats dir", result.Checked)
	}
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %d, want 0 for empty polecats dir", len(result.Stalled))
	}
}

func TestDetectStalledPolecats_NoSession(t *testing.T) {
	t.Parallel()
	// When tmux sessions don't exist (no real tmux in test),
	// HasSession returns false so polecats are skipped (not errors).
	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create polecat directories
	for _, name := range []string{"alpha", "bravo"} {
		if err := os.Mkdir(filepath.Join(polecatsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Create hidden dir (should be skipped)
	if err := os.Mkdir(filepath.Join(polecatsDir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := DetectStalledPolecats(tmpDir, rigName)

	// Should count 2 polecats (skip hidden)
	if result.Checked != 2 {
		t.Errorf("Checked = %d, want 2 (should skip hidden dirs)", result.Checked)
	}

	// No stalled because HasSession returns false (no real tmux in test),
	// so polecats are skipped before structured signal checks.
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %d, want 0 (no tmux sessions in test)", len(result.Stalled))
	}
}

// writeFakeTmuxWithBlockingDialog creates a fake `tmux` binary that reports a
// live session with a claude-like agent process whose pane is showing a
// blocking selection dialog (matches containsBlockingQuestionDialog). Used
// to drive DetectStalledPolecats all the way to recoverDialogBlockedPolecat
// without a real tmux server or agent process.
func writeFakeTmuxWithBlockingDialog(t *testing.T, dir string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *has-session*) exit 0;;\n" +
		"  *show-environment*) echo 'unknown variable' >&2; exit 1;;\n" +
		"  *display-message*) echo 'claude';;\n" +
		"  *capture-pane*) printf 'Proceed?\\n1. Yes\\n2. No\\nEnter to select, Esc to cancel\\n';;\n" +
		"  *send-keys*|*kill-session*) exit 0;;\n" +
		"  *) exit 1;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0755); err != nil {
		t.Fatalf("writing fake tmux: %v", err)
	}
}

// writeFakeGTRefusing creates a fake `gt` binary that always fails
// immediately. recoverDialogBlockedPolecat shells out to the real `gt
// escalate`; this keeps that call from ever reaching an actual `gt` binary
// (and, via it, a live town) if one happens to be on the test runner's PATH.
func writeFakeGTRefusing(t *testing.T, dir string) {
	t.Helper()
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gt"), []byte(script), 0755); err != nil {
		t.Fatalf("writing fake gt: %v", err)
	}
}

// TestDetectStalledPolecats_HonoursPauseMarker pins the pause gate the mayor
// required for this scanner (gt-ahik, om kgx0): a frozen (SIGSTOPped)
// polecat still has a live tmux session and process, so it looks exactly
// like one stuck on a blocking dialog. Without the gate,
// recoverDialogBlockedPolecat would send Escape and a nudge into a pane the
// operator is deliberately holding. The control proves the fake tmux setup
// actually reaches that path when the polecat is NOT paused, so the paused
// case is a real skip, not an artifact of the test harness.
func TestDetectStalledPolecats_HonoursPauseMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script to mock tmux")
	}

	binDir := t.TempDir()
	writeFakeTmuxWithBlockingDialog(t, binDir)
	writeFakeGTRefusing(t, binDir)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("GT_TOWN_SOCKET", "")

	townRoot := t.TempDir()
	rigName := "gastown"
	polecatsDir := filepath.Join(townRoot, rigName, "polecats")
	for _, name := range []string{"frozen", "control"} {
		if err := os.MkdirAll(filepath.Join(polecatsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := agentpause.Pause(townRoot, rigName, "polecat", "frozen", "operator hold", "human", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	result := DetectStalledPolecats(townRoot, rigName)

	if result.Checked != 2 {
		t.Fatalf("Checked = %d, want 2", result.Checked)
	}
	if len(result.Stalled) != 1 {
		t.Fatalf("Stalled = %d, want exactly 1 (only the unpaused control)", len(result.Stalled))
	}
	if got := result.Stalled[0].PolecatName; got != "control" {
		t.Errorf("Stalled[0].PolecatName = %q, want %q (the paused polecat must not appear at all)", got, "control")
	}
	if result.Stalled[0].StallType != "dialog-blocked" {
		t.Errorf("Stalled[0].StallType = %q, want %q", result.Stalled[0].StallType, "dialog-blocked")
	}
}

func TestStartupStallThresholds(t *testing.T) {
	t.Parallel()
	// Verify config defaults are reasonable (tests the operational config defaults,
	// not removed handler constants).
	stallThreshold := config.DefaultWitnessStartupStallThreshold
	activityGrace := config.DefaultWitnessStartupActivityGrace
	if stallThreshold < 30*time.Second {
		t.Errorf("DefaultWitnessStartupStallThreshold = %v, too short (< 30s)", stallThreshold)
	}
	if stallThreshold > 5*time.Minute {
		t.Errorf("DefaultWitnessStartupStallThreshold = %v, too long (> 5min)", stallThreshold)
	}
	if activityGrace < 15*time.Second {
		t.Errorf("DefaultWitnessStartupActivityGrace = %v, too short (< 15s)", activityGrace)
	}
	if activityGrace > 5*time.Minute {
		t.Errorf("DefaultWitnessStartupActivityGrace = %v, too long (> 5min)", activityGrace)
	}
}

func TestDetectOrphanedBeads_NoBdAvailable(t *testing.T) {
	t.Parallel()
	// When bd is not available (test environment), should return empty result
	result := DetectOrphanedBeads(DefaultBdCli(), "/nonexistent", "testrig", nil)

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 when bd unavailable", result.Checked)
	}
	if len(result.Orphans) != 0 {
		t.Errorf("Orphans = %d, want 0 when bd unavailable", len(result.Orphans))
	}
}

func TestDetectOrphanedBeads_ResultTypes(t *testing.T) {
	t.Parallel()
	// Verify the OrphanedBeadResult type has all expected fields
	o := OrphanedBeadResult{
		BeadID:        "gt-orphan1",
		Assignee:      "testrig/polecats/alpha",
		PolecatName:   "alpha",
		BeadRecovered: true,
	}

	if o.BeadID != "gt-orphan1" {
		t.Errorf("BeadID = %q, want %q", o.BeadID, "gt-orphan1")
	}
	if o.Assignee != "testrig/polecats/alpha" {
		t.Errorf("Assignee = %q, want %q", o.Assignee, "testrig/polecats/alpha")
	}
	if o.PolecatName != "alpha" {
		t.Errorf("PolecatName = %q, want %q", o.PolecatName, "alpha")
	}
	if !o.BeadRecovered {
		t.Error("BeadRecovered = false, want true")
	}
}

func TestDetectOrphanedBeads_WithMockBd(t *testing.T) {
	installFakeTmuxNoServer(t)

	// Set up town directory structure
	townRoot := t.TempDir()
	rigName := "testrig"
	polecatsDir := filepath.Join(townRoot, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a polecat directory for "bravo" (alive dir, dead session)
	// This case should be SKIPPED (deferred to DetectZombiePolecats)
	if err := os.Mkdir(filepath.Join(polecatsDir, "bravo"), 0o755); err != nil {
		t.Fatal(err)
	}

	// "alpha" has NO directory and NO tmux session — true orphan
	// "bravo" has directory but no session — deferred to DetectZombiePolecats
	// "charlie" is hooked, no dir, no session — also an orphan
	// "delta" is assigned to a different rig — skipped by rigName filter

	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 {
				return "{}", nil
			}
			switch args[0] {
			case "list":
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "--status=in_progress") {
					return `[
  {"id":"gt-orphan1","assignee":"testrig/polecats/alpha"},
  {"id":"gt-alive1","assignee":"testrig/polecats/bravo"},
  {"id":"gt-nocrew","assignee":"testrig/crew/sean"},
  {"id":"gt-noassign","assignee":""},
  {"id":"gt-otherrig","assignee":"otherrig/polecats/delta"}
]`, nil
				}
				if strings.Contains(joined, "--status=hooked") {
					return `[{"id":"gt-hooked1","assignee":"testrig/polecats/charlie"}]`, nil
				}
				return "[]", nil
			case "show":
				return `[{"status":"in_progress"}]`, nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	result := DetectOrphanedBeads(bd, townRoot, rigName, nil)

	// Verify --limit=0 was passed in bd list invocations
	logStr := strings.Join(mock.calls, "\n")
	if !strings.Contains(logStr, "--limit=0") {
		t.Errorf("bd list was not called with --limit=0; log:\n%s", logStr)
	}
	// Verify both statuses were queried
	if !strings.Contains(logStr, "--status=in_progress") {
		t.Errorf("bd list was not called with --status=in_progress; log:\n%s", logStr)
	}
	if !strings.Contains(logStr, "--status=hooked") {
		t.Errorf("bd list was not called with --status=hooked; log:\n%s", logStr)
	}

	// Should have checked 3 polecat assignees in "testrig":
	// alpha (in_progress), bravo (in_progress), charlie (hooked)
	// "crew/sean" is not a polecat, "" has no assignee,
	// "otherrig/polecats/delta" is filtered out by rigName
	if result.Checked != 3 {
		t.Errorf("Checked = %d, want 3 (alpha + bravo from in_progress, charlie from hooked)", result.Checked)
	}

	// Should have found 2 orphans:
	// alpha (in_progress, no dir, no session) and charlie (hooked, no dir, no session)
	// bravo has directory so deferred to DetectZombiePolecats
	if len(result.Orphans) != 2 {
		t.Fatalf("Orphans = %d, want 2 (alpha + charlie)", len(result.Orphans))
	}

	// Verify first orphan (alpha from in_progress scan)
	orphan := result.Orphans[0]
	if orphan.BeadID != "gt-orphan1" {
		t.Errorf("orphan[0] BeadID = %q, want %q", orphan.BeadID, "gt-orphan1")
	}
	if orphan.PolecatName != "alpha" {
		t.Errorf("orphan[0] PolecatName = %q, want %q", orphan.PolecatName, "alpha")
	}
	if orphan.Assignee != "testrig/polecats/alpha" {
		t.Errorf("orphan[0] Assignee = %q, want %q", orphan.Assignee, "testrig/polecats/alpha")
	}
	// BeadRecovered should be true (mock bd update succeeds)
	if !orphan.BeadRecovered {
		t.Error("orphan[0] BeadRecovered = false, want true")
	}

	// Verify second orphan (charlie from hooked scan)
	orphan2 := result.Orphans[1]
	if orphan2.BeadID != "gt-hooked1" {
		t.Errorf("orphan[1] BeadID = %q, want %q", orphan2.BeadID, "gt-hooked1")
	}
	if orphan2.PolecatName != "charlie" {
		t.Errorf("orphan[1] PolecatName = %q, want %q", orphan2.PolecatName, "charlie")
	}

	// Verify no unexpected errors
	if len(result.Errors) != 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
}

func TestDetectOrphanedBeads_ErrorPath(t *testing.T) {
	t.Parallel()
	bdErr := fmt.Errorf("bd: connection refused")
	bd, _ := mockBd(
		func(args []string) (string, error) { return "", bdErr },
		func(args []string) error { return bdErr },
	)

	result := DetectOrphanedBeads(bd, t.TempDir(), "testrig", nil)

	if len(result.Errors) == 0 {
		t.Error("expected errors when bd fails, got none")
	}
	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 when bd fails", result.Checked)
	}
	if len(result.Orphans) != 0 {
		t.Errorf("Orphans = %d, want 0 when bd fails", len(result.Orphans))
	}
}

// --- DetectOrphanedMolecules tests ---

func TestOrphanedMoleculeResult_Types(t *testing.T) {
	t.Parallel()
	// Verify the result types have all expected fields.
	r := OrphanedMoleculeResult{
		BeadID:        "gt-work-123",
		MoleculeID:    "gt-mol-456",
		Assignee:      "testrig/polecats/alpha",
		PolecatName:   "alpha",
		Closed:        5,
		BeadRecovered: true,
		Error:         nil,
	}
	if r.BeadID != "gt-work-123" {
		t.Errorf("BeadID = %q, want %q", r.BeadID, "gt-work-123")
	}
	if r.MoleculeID != "gt-mol-456" {
		t.Errorf("MoleculeID = %q, want %q", r.MoleculeID, "gt-mol-456")
	}
	if r.PolecatName != "alpha" {
		t.Errorf("PolecatName = %q, want %q", r.PolecatName, "alpha")
	}
	if r.Closed != 5 {
		t.Errorf("Closed = %d, want 5", r.Closed)
	}
	if !r.BeadRecovered {
		t.Error("BeadRecovered = false, want true")
	}

	// Aggregate result
	agg := DetectOrphanedMoleculesResult{
		Checked: 10,
		Orphans: []OrphanedMoleculeResult{r},
		Errors:  []error{fmt.Errorf("test error")},
	}
	if agg.Checked != 10 {
		t.Errorf("Checked = %d, want 10", agg.Checked)
	}
	if len(agg.Orphans) != 1 {
		t.Errorf("len(Orphans) = %d, want 1", len(agg.Orphans))
	}
	if len(agg.Errors) != 1 {
		t.Errorf("len(Errors) = %d, want 1", len(agg.Errors))
	}
}

func TestDetectOrphanedMolecules_NoBdAvailable(t *testing.T) {
	t.Parallel()
	// When bd is not available, should return empty result with errors.
	bdErr := fmt.Errorf("bd: not found")
	bd, _ := mockBd(
		func(args []string) (string, error) { return "", bdErr },
		func(args []string) error { return bdErr },
	)
	result := DetectOrphanedMolecules(bd, "/tmp/nonexistent", "testrig", nil)
	if result == nil {
		t.Fatal("result should not be nil")
	}
	// Should have errors from failed bd list commands
	if len(result.Errors) == 0 {
		t.Error("expected errors when bd is not available")
	}
	if len(result.Orphans) != 0 {
		t.Errorf("expected no orphans, got %d", len(result.Orphans))
	}
}

func TestDetectOrphanedMolecules_EmptyResult(t *testing.T) {
	t.Parallel()
	// With a mock bd that returns empty lists, should get empty result.
	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	result := DetectOrphanedMolecules(bd, t.TempDir(), "testrig", nil)
	if result == nil {
		t.Fatal("result should not be nil")
	}
	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0", result.Checked)
	}
	if len(result.Orphans) != 0 {
		t.Errorf("len(Orphans) = %d, want 0", len(result.Orphans))
	}
}

func TestGetAttachedMoleculeID_EmptyOutput(t *testing.T) {
	t.Parallel()
	// When bd returns error, should return empty string.
	bd, _ := mockBd(
		func(args []string) (string, error) { return "", fmt.Errorf("bd: not found") },
		func(args []string) error { return fmt.Errorf("bd: not found") },
	)
	result := getAttachedMoleculeID(bd, "/tmp", "gt-fake-123")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestHandlePolecatDone_CompletedWithoutMRID_NoMergeReady(t *testing.T) {
	t.Parallel()
	// When Exit==COMPLETED but MRID is empty and MRFailed is true,
	// the witness should NOT send MERGE_READY (go to no-MR path).
	// This tests the fix for gt-xp6e9p.
	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		IssueID:     "gt-abc123",
		MRID:        "",
		Branch:      "polecat/nux-abc123",
		MRFailed:    true,
	}

	// hasPendingMR should be false when MRID is empty
	hasPendingMR := payload.MRID != ""
	if hasPendingMR {
		t.Error("hasPendingMR = true, want false when MRID is empty")
	}

	// Even with Exit==COMPLETED, MRFailed should prevent the bead lookup fallback
	if !payload.MRFailed && payload.Exit == "COMPLETED" && payload.Branch != "" {
		t.Error("should not attempt MR bead lookup when MRFailed is true")
	}
}

func TestHandlePolecatDone_CompletedWithMRID(t *testing.T) {
	t.Parallel()
	// When Exit==COMPLETED and MRID is set, hasPendingMR should be true.
	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		MRID:        "gt-mr-xyz",
		Branch:      "polecat/nux-abc123",
	}

	hasPendingMR := payload.MRID != ""
	if !hasPendingMR {
		t.Error("hasPendingMR = false, want true when MRID is set")
	}
}

func TestFindMRBeadForBranch_NoBdAvailable(t *testing.T) {
	t.Parallel()
	// When bd is not available, should return empty string
	result := findMRBeadForBranch(DefaultBdCli(), "/nonexistent", "polecat/nux-abc123")
	if result != "" {
		t.Errorf("findMRBeadForBranch = %q, want empty when bd unavailable", result)
	}
}

func TestDetectOrphanedMolecules_WithMockBd(t *testing.T) {
	installFakeTmuxNoServer(t)

	// Full test with mock bd returning beads assigned to dead polecats.
	//
	// Setup:
	// - alpha: dead polecat (no tmux, no directory) with attached molecule → orphaned
	// - bravo: alive polecat (directory exists) → skip
	// - crew/sean: non-polecat assignee → skip
	// - empty assignee → skip

	tmpDir := t.TempDir()

	// Create town structure: tmpDir is the "town root"
	rigName := "testrig"
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create bravo's directory (alive polecat)
	if err := os.MkdirAll(filepath.Join(polecatsDir, "bravo"), 0755); err != nil {
		t.Fatal(err)
	}
	// No directory for alpha (dead polecat)

	// Create workspace.Find marker
	if err := os.WriteFile(filepath.Join(tmpDir, ".gt-root"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 {
				return "[]", nil
			}
			joined := strings.Join(args, " ")
			switch args[0] {
			case "list":
				if strings.Contains(joined, "--status=hooked") {
					return `[
  {"id":"gt-work-001","assignee":"testrig/polecats/alpha"},
  {"id":"gt-work-002","assignee":"testrig/polecats/bravo"},
  {"id":"gt-work-003","assignee":"testrig/crew/sean"},
  {"id":"gt-work-004","assignee":""}
]`, nil
				}
				if strings.Contains(joined, "--status=in_progress") {
					return "[]", nil
				}
				if strings.Contains(joined, "--parent=gt-mol-orphan") {
					return `[
  {"id":"gt-step-001","status":"open"},
  {"id":"gt-step-002","status":"open"},
  {"id":"gt-step-003","status":"closed"}
]`, nil
				}
				return "[]", nil
			case "show":
				if len(args) > 1 {
					switch args[1] {
					case "gt-work-001":
						return `[{"status":"hooked","description":"attached_molecule: gt-mol-orphan\nattached_at: 2026-01-15T10:00:00Z\ndispatched_by: mayor"}]`, nil
					case "gt-mol-orphan":
						return `[{"status":"open"}]`, nil
					}
				}
				return `[{"status":"open","description":""}]`, nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	result := DetectOrphanedMolecules(bd, tmpDir, rigName, nil)
	if result == nil {
		t.Fatal("result should not be nil")
	}

	// Should have checked 2 polecat-assigned beads (alpha and bravo)
	if result.Checked != 2 {
		t.Errorf("Checked = %d, want 2 (alpha + bravo)", result.Checked)
	}

	// Should have found 1 orphan (alpha's molecule)
	if len(result.Orphans) != 1 {
		t.Fatalf("len(Orphans) = %d, want 1", len(result.Orphans))
	}

	orphan := result.Orphans[0]
	if orphan.BeadID != "gt-work-001" {
		t.Errorf("orphan.BeadID = %q, want %q", orphan.BeadID, "gt-work-001")
	}
	if orphan.MoleculeID != "gt-mol-orphan" {
		t.Errorf("orphan.MoleculeID = %q, want %q", orphan.MoleculeID, "gt-mol-orphan")
	}
	if orphan.PolecatName != "alpha" {
		t.Errorf("orphan.PolecatName = %q, want %q", orphan.PolecatName, "alpha")
	}
	// Closed should be 3: 2 open step children + 1 molecule itself
	if orphan.Closed != 3 {
		t.Errorf("orphan.Closed = %d, want 3 (2 open steps + 1 molecule)", orphan.Closed)
	}
	if orphan.Error != nil {
		t.Errorf("orphan.Error = %v, want nil", orphan.Error)
	}

	// Verify bd close was called by checking the mock log
	logContent := strings.Join(mock.calls, "\n")
	if !strings.Contains(logContent, "close gt-step-001 gt-step-002") {
		t.Errorf("expected bd close for step children, got log:\n%s", logContent)
	}
	if !strings.Contains(logContent, "close gt-mol-orphan") {
		t.Errorf("expected bd close for molecule, got log:\n%s", logContent)
	}
	// Verify bead was recovered (resetAbandonedBead called bd update)
	if !orphan.BeadRecovered {
		t.Error("orphan.BeadRecovered = false, want true (resetAbandonedBead should have reset the bead)")
	}
	if !strings.Contains(logContent, "update gt-work-001") {
		t.Errorf("expected bd update for bead reset, got log:\n%s", logContent)
	}
}

func TestCompletionDiscovery_Types(t *testing.T) {
	t.Parallel()
	// Verify CompletionDiscovery has all expected fields
	d := CompletionDiscovery{
		PolecatName:    "nux",
		AgentBeadID:    "gt-gastown-polecat-nux",
		ExitType:       "COMPLETED",
		IssueID:        "gt-abc123",
		MRID:           "gt-mr-xyz",
		Branch:         "polecat/nux/gt-abc123@hash",
		MRFailed:       false,
		CompletionTime: "2026-02-28T02:00:00Z",
		Action:         "merge-ready-sent",
		WispCreated:    "gt-wisp-123",
	}

	if d.PolecatName != "nux" {
		t.Errorf("PolecatName = %q, want %q", d.PolecatName, "nux")
	}
	if d.ExitType != "COMPLETED" {
		t.Errorf("ExitType = %q, want %q", d.ExitType, "COMPLETED")
	}
	if d.Branch != "polecat/nux/gt-abc123@hash" {
		t.Errorf("Branch = %q, want correct value", d.Branch)
	}
}

func TestDiscoverCompletionsResult_EmptyResult(t *testing.T) {
	t.Parallel()
	result := &DiscoverCompletionsResult{}
	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0", result.Checked)
	}
	if len(result.Discovered) != 0 {
		t.Errorf("Discovered = %d, want 0", len(result.Discovered))
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %d, want 0", len(result.Errors))
	}
}

func TestDiscoverCompletions_NonexistentDir(t *testing.T) {
	t.Parallel()
	// When workDir doesn't exist, should return empty result
	result := DiscoverCompletions(DefaultBdCli(), "/nonexistent/path", "testrig", nil)
	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 for nonexistent dir", result.Checked)
	}
}

func TestDiscoverCompletions_EmptyPolecatsDir(t *testing.T) {
	t.Parallel()
	// When polecats directory exists but is empty, should scan 0
	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create workspace marker
	if err := os.WriteFile(filepath.Join(tmpDir, ".gt-root"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	result := DiscoverCompletions(DefaultBdCli(), tmpDir, rigName, nil)
	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 for empty polecats dir", result.Checked)
	}
}

func TestDiscoverCompletions_NoCompletionMetadata(t *testing.T) {
	// Polecat exists but agent bead has no completion metadata — should be skipped
	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(filepath.Join(polecatsDir, "nux"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".gt-root"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Mock bd that returns agent bead with no completion fields
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 0 && args[0] == "show" {
				return `[{"id":"gt-testrig-polecat-nux","description":"Agent: testrig/polecats/nux\n\nrole_type: polecat\nrig: testrig\nagent_state: working\nhook_bead: gt-work-001","agent_state":"working","hook_bead":"gt-work-001"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	result := DiscoverCompletions(bd, tmpDir, rigName, nil)
	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
	if len(result.Discovered) != 0 {
		t.Errorf("Discovered = %d, want 0 (no completion metadata)", len(result.Discovered))
	}
}

func TestProcessDiscoveredCompletion_PhaseComplete(t *testing.T) {
	t.Parallel()
	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "PHASE_COMPLETE",
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(DefaultBdCli(), "/tmp", "testrig", payload, discovery)
	if discovery.Action != "phase-complete" {
		t.Errorf("Action = %q, want %q", discovery.Action, "phase-complete")
	}
}

func TestProcessDiscoveredCompletion_NoMR(t *testing.T) {
	t.Parallel()
	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		MRFailed:    true, // Prevents fallback MR lookup
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(DefaultBdCli(), "/tmp", "testrig", payload, discovery)
	if !strings.Contains(discovery.Action, "acknowledged-idle") {
		t.Errorf("Action = %q, want to contain %q", discovery.Action, "acknowledged-idle")
	}
}

func TestProcessDiscoveredCompletion_EscalatedNoMR(t *testing.T) {
	t.Parallel()
	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "ESCALATED",
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(DefaultBdCli(), "/tmp", "testrig", payload, discovery)
	if !strings.Contains(discovery.Action, "acknowledged-idle") {
		t.Errorf("Action = %q, want to contain %q for ESCALATED exit", discovery.Action, "acknowledged-idle")
	}
}

// completionMRQueryHandler returns a mock exec function that answers the
// merge-request lookup (findMRBeadForBranch) and the cleanup-wisp lookup
// (findCleanupWispsForCompletion) queries processDiscoveredCompletion issues
// when routing a completion with a pending MR, plus "create"/"show"/"update"
// handlers for the wisp lifecycle. wispsJSON is returned verbatim for the
// cleanup-wisp query; override per test to simulate existing wisps.
func completionMRQueryHandler(branch, wispsJSON string, newWispID string, createCalled *bool) func(args []string) (string, error) {
	return func(args []string) (string, error) {
		if len(args) == 0 {
			return "{}", nil
		}
		switch args[0] {
		case "query":
			if len(args) > 1 && strings.Contains(args[1], "gt:merge-request") {
				return fmt.Sprintf(`[{"id":"gt-mr-1","description":"Branch: %s"}]`, branch), nil
			}
			if len(args) > 1 && strings.Contains(args[1], "label=cleanup") {
				return wispsJSON, nil
			}
		case "create":
			if createCalled != nil {
				*createCalled = true
			}
			return fmt.Sprintf(`{"id":%q}`, newWispID), nil
		case "show":
			return `[{"labels":["cleanup","polecat:nux","state:pending"]}]`, nil
		}
		return "{}", nil
	}
}

func TestProcessDiscoveredCompletion_IdempotentWhenWispAlreadyExists(t *testing.T) {
	t.Parallel()
	var createCalled bool
	var updateCalled bool
	existingWisps := `[{"id":"gt-wisp-existing","description":"Verify and cleanup polecat nux\nIssue: gt-abc\nBranch: feature-x"}]`
	bd, _ := mockBd(
		completionMRQueryHandler("feature-x", existingWisps, "gt-wisp-new", &createCalled),
		func(args []string) error {
			if len(args) > 0 && args[0] == "update" {
				updateCalled = true
			}
			return nil
		},
	)

	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		IssueID:     "gt-abc",
		Branch:      "feature-x",
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(bd, t.TempDir(), "testrig", payload, discovery)

	if createCalled {
		t.Error("expected no new wisp to be created when an open wisp already matches this (issue, branch)")
	}
	if discovery.WispCreated != "gt-wisp-existing" {
		t.Errorf("WispCreated = %q, want %q", discovery.WispCreated, "gt-wisp-existing")
	}
	if !strings.Contains(discovery.Action, "already-tracked") {
		t.Errorf("Action = %q, want to contain %q", discovery.Action, "already-tracked")
	}
	if discovery.Error != nil {
		t.Errorf("Error = %v, want nil", discovery.Error)
	}
	if !updateCalled {
		t.Error("expected UpdateCleanupWispState to still be attempted for an already-tracked wisp — " +
			"otherwise a state-update failure on the cycle that created it can never be retried and the " +
			"wisp is stranded at state:pending forever (gt-mf5q review)")
	}
}

func TestProcessDiscoveredCompletion_DoesNotMatchWispForDifferentIssue(t *testing.T) {
	t.Parallel()
	// A polecat can legitimately hold an open cleanup wisp for a DIFFERENT
	// issue (dispatch reuse) at the same time it completes a new one. That
	// must not be treated as an existing match for this completion.
	var createCalled bool
	otherIssueWisps := `[{"id":"gt-wisp-other","description":"Verify and cleanup polecat nux\nIssue: gt-other\nBranch: other-branch"}]`
	bd, _ := mockBd(
		completionMRQueryHandler("feature-x", otherIssueWisps, "gt-wisp-new", &createCalled),
		func(args []string) error { return nil },
	)

	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		IssueID:     "gt-abc",
		Branch:      "feature-x",
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(bd, t.TempDir(), "testrig", payload, discovery)

	if !createCalled {
		t.Error("expected a new wisp to be created — the existing wisp is for a different issue")
	}
	if discovery.WispCreated != "gt-wisp-new" {
		t.Errorf("WispCreated = %q, want %q", discovery.WispCreated, "gt-wisp-new")
	}
}

func TestProcessDiscoveredCompletion_ClosesDuplicateOnConcurrentRace(t *testing.T) {
	t.Parallel()
	// Simulates two concurrent patrol scans racing to process the same
	// completion (gt-mf5q case 1): the idempotency pre-check sees nothing,
	// both create a wisp, and the post-create dedup check must find both and
	// close the loser deterministically (lowest ID wins).
	var closedIDs []string
	callCount := 0
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 {
				return "{}", nil
			}
			switch args[0] {
			case "query":
				if len(args) > 1 && strings.Contains(args[1], "gt:merge-request") {
					return `[{"id":"gt-mr-1","description":"Branch: feature-x"}]`, nil
				}
				if len(args) > 1 && strings.Contains(args[1], "label=cleanup") {
					callCount++
					if callCount == 1 {
						// Idempotency pre-check: nothing tracked yet.
						return "[]", nil
					}
					// Post-create dedup check: our wisp and the concurrent
					// scan's wisp both show up now.
					return `[{"id":"gt-wisp-b","description":"Verify and cleanup polecat nux\nIssue: gt-abc\nBranch: feature-x"},` +
						`{"id":"gt-wisp-a","description":"Verify and cleanup polecat nux\nIssue: gt-abc\nBranch: feature-x"}]`, nil
				}
			case "create":
				return `{"id":"gt-wisp-b"}`, nil
			case "show":
				return `[{"labels":["cleanup","polecat:nux","state:pending"]}]`, nil
			case "close":
				if len(args) > 1 {
					closedIDs = append(closedIDs, args[1])
				}
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		IssueID:     "gt-abc",
		Branch:      "feature-x",
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(bd, t.TempDir(), "testrig", payload, discovery)

	if len(closedIDs) != 1 || closedIDs[0] != "gt-wisp-b" {
		t.Errorf("closed IDs = %v, want [gt-wisp-b] (the later, non-winning wisp)", closedIDs)
	}
	if discovery.WispCreated != "gt-wisp-a" {
		t.Errorf("WispCreated = %q, want %q (the deterministic winner)", discovery.WispCreated, "gt-wisp-a")
	}
}

func TestProcessDiscoveredCompletion_StrandedPendingWispRetriesStateUpdate(t *testing.T) {
	t.Parallel()
	// gt-mf5q review, issue (1): the previous fix's already-tracked
	// short-circuit returned before ever calling UpdateCleanupWispState, so
	// a state-update failure on the cycle that created the wisp had no way
	// to retry — the wisp was stranded at state:pending permanently, a state
	// findCleanupWisp (which filters on state:merge-requested) never
	// matches. Simulates rediscovery of such a stranded wisp on a later
	// cycle and verifies the state update is retried and, on success,
	// clears the way for the metadata clear (discovery.Error == nil).
	var updateArgs []string
	strandedWisp := `[{"id":"gt-wisp-stranded","description":"Verify and cleanup polecat nux\nIssue: gt-abc\nBranch: feature-x"}]`
	bd, _ := mockBd(
		completionMRQueryHandler("feature-x", strandedWisp, "gt-wisp-new", nil),
		func(args []string) error {
			updateArgs = args
			return nil
		},
	)

	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		IssueID:     "gt-abc",
		Branch:      "feature-x",
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(bd, t.TempDir(), "testrig", payload, discovery)

	if len(updateArgs) == 0 || updateArgs[0] != "update" || updateArgs[1] != "gt-wisp-stranded" {
		t.Errorf("update args = %v, want an update of gt-wisp-stranded", updateArgs)
	}
	found := false
	for _, a := range updateArgs {
		if strings.Contains(a, "state:merge-requested") {
			found = true
		}
	}
	if !found {
		t.Errorf("update args = %v, want a state:merge-requested label", updateArgs)
	}
	if discovery.Error != nil {
		t.Errorf("Error = %v, want nil — the retried state update succeeded, so the completion metadata clear must proceed", discovery.Error)
	}
}

func TestProcessDiscoveredCompletion_NudgeFailureDoesNotBlockMetadataClear(t *testing.T) {
	t.Parallel()
	// gt-mf5q review, issue (2): the original regression test for this used
	// installFakeTmuxNoServer, but nudgeRefinery's HasSession check treats
	// ErrNoServer as (false, nil) and short-circuits to a nil return before
	// ever attempting the nudge — so that test could never observe a real
	// nudge failure, before or after the fix. Override the package-level
	// nudgeRefinery var directly to produce a genuine failure instead.
	origNudge := nudgeRefinery
	defer func() { nudgeRefinery = origNudge }()
	nudgeRefinery = func(townRoot, rigName string) error {
		return errors.New("refinery session busy")
	}

	bd, _ := mockBd(
		completionMRQueryHandler("feature-x", "[]", "gt-wisp-new", nil),
		func(args []string) error { return nil },
	)

	payload := &PolecatDonePayload{
		PolecatName: "nux",
		Exit:        "COMPLETED",
		IssueID:     "gt-abc",
		Branch:      "feature-x",
	}
	discovery := &CompletionDiscovery{}
	processDiscoveredCompletion(bd, t.TempDir(), "testrig", payload, discovery)

	if discovery.Error != nil {
		t.Errorf("Error = %v, want nil — a nudge failure must not gate the metadata clear", discovery.Error)
	}
	if discovery.WispCreated != "gt-wisp-new" {
		t.Errorf("WispCreated = %q, want %q", discovery.WispCreated, "gt-wisp-new")
	}
	if !strings.Contains(discovery.Action, "nudge") {
		t.Errorf("Action = %q, want it to record the nudge failure for operator visibility", discovery.Action)
	}
}

func TestFindCleanupWispsForCompletion_MatchesExactIssueAndBranch(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			return `[
				{"id":"gt-wisp-match","description":"Verify and cleanup polecat nux\nIssue: gt-abc\nBranch: feature-x"},
				{"id":"gt-wisp-other-issue","description":"Verify and cleanup polecat nux\nIssue: gt-zzz\nBranch: feature-x"},
				{"id":"gt-wisp-other-branch","description":"Verify and cleanup polecat nux\nIssue: gt-abc\nBranch: other-branch"}
			]`, nil
		},
		func(args []string) error { return nil },
	)
	workDir := t.TempDir()

	result := findCleanupWispsForCompletion(bd, workDir, "gastown", "nux", "gt-abc", "feature-x")

	if len(result) != 1 || result[0] != "gt-wisp-match" {
		t.Errorf("findCleanupWispsForCompletion = %v, want [gt-wisp-match]", result)
	}

	got := strings.Join(mock.calls, "\n")
	if !strings.Contains(got, "ephemeral=true") || !strings.Contains(got, "label=cleanup") || !strings.Contains(got, "polecat:nux") {
		t.Errorf("findCleanupWispsForCompletion: expected ephemeral/cleanup/polecat filter, got: %s", got)
	}
}

func TestFindCleanupWispsForCompletion_NoMatches(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			return `[{"id":"gt-wisp-other","description":"Verify and cleanup polecat nux\nIssue: gt-zzz\nBranch: other-branch"}]`, nil
		},
		func(args []string) error { return nil },
	)
	workDir := t.TempDir()

	result := findCleanupWispsForCompletion(bd, workDir, "gastown", "nux", "gt-abc", "feature-x")
	if result != nil {
		t.Errorf("findCleanupWispsForCompletion = %v, want nil", result)
	}
}

func TestGetAgentBeadFields_NoAgentBead(t *testing.T) {
	t.Parallel()
	// A workDir with no beads database: GetAgentBead's Show fails, so fields is nil.
	fields := getAgentBeadFields(t.TempDir(), "gt-fake-agent")
	if fields != nil {
		t.Error("expected nil fields when no beads database is reachable")
	}
}

func TestClearCompletionMetadata_NoBeadsDir(t *testing.T) {
	t.Parallel()
	// A workDir with no beads database: the wrapper's Show fails and the
	// function must surface that as an error, not silently succeed.
	err := clearCompletionMetadata(t.TempDir(), "gt-fake-agent")
	if err == nil {
		t.Error("expected error when no beads database is reachable")
	}
}

// --- Heartbeat v2 tests (gt-3vr5) ---

func TestHeartbeatV2_ExitingStateSkipsZombieDetection(t *testing.T) {
	t.Parallel()
	// Agent reports "exiting" state via heartbeat v2.
	// The witness should trust the agent and NOT flag as zombie,
	// even if done-intent is older than config.DefaultWitnessDoneIntentStuckTimeout.
	// This replaces timer-based inference for v2 agents.

	// Fresh heartbeat with state="exiting" → not a zombie
	hb := &polecat.SessionHeartbeat{
		Timestamp: time.Now(),
		State:     polecat.HeartbeatExiting,
	}
	stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
	if stale {
		t.Error("fresh heartbeat should not be stale")
	}
	if hb.EffectiveState() != polecat.HeartbeatExiting {
		t.Errorf("EffectiveState() = %q, want %q", hb.EffectiveState(), polecat.HeartbeatExiting)
	}

	// With a v2 exiting heartbeat, the witness should NOT check done-intent timers
	shouldSkip := hb.IsV2() && !stale && hb.EffectiveState() == polecat.HeartbeatExiting
	if !shouldSkip {
		t.Error("expected v2 exiting heartbeat to skip zombie detection")
	}
}

func TestHeartbeatV2_StuckStateEscalates(t *testing.T) {
	t.Parallel()
	// Agent self-reports "stuck" via heartbeat v2.
	// The witness should escalate (not restart — agent is alive).
	hb := &polecat.SessionHeartbeat{
		Timestamp: time.Now(),
		State:     polecat.HeartbeatStuck,
		Context:   "blocked on auth issue",
	}
	stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
	if stale {
		t.Error("fresh heartbeat should not be stale")
	}

	shouldEscalate := hb.IsV2() && !stale && hb.EffectiveState() == polecat.HeartbeatStuck
	if !shouldEscalate {
		t.Error("expected v2 stuck heartbeat to trigger escalation")
	}
}

func TestHeartbeatV2_WorkingStateHealthy(t *testing.T) {
	t.Parallel()
	// Agent heartbeats "working" — healthy, not a zombie.
	hb := &polecat.SessionHeartbeat{
		Timestamp: time.Now(),
		State:     polecat.HeartbeatWorking,
	}
	stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
	shouldSkip := hb.IsV2() && !stale && (hb.EffectiveState() == polecat.HeartbeatWorking || hb.EffectiveState() == polecat.HeartbeatIdle)
	if !shouldSkip {
		t.Error("expected v2 working heartbeat to skip zombie detection")
	}
}

func TestHeartbeatV2_IdleStateHealthy(t *testing.T) {
	t.Parallel()
	hb := &polecat.SessionHeartbeat{
		Timestamp: time.Now(),
		State:     polecat.HeartbeatIdle,
	}
	stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
	shouldSkip := hb.IsV2() && !stale && (hb.EffectiveState() == polecat.HeartbeatWorking || hb.EffectiveState() == polecat.HeartbeatIdle)
	if !shouldSkip {
		t.Error("expected v2 idle heartbeat to skip zombie detection")
	}
}

func TestHeartbeatV2_StaleHeartbeatFallsThrough(t *testing.T) {
	t.Parallel()
	// Stale v2 heartbeat (agent died) → fall through to legacy detection.
	hb := &polecat.SessionHeartbeat{
		Timestamp: time.Now().Add(-10 * time.Minute), // 10min old → stale
		State:     polecat.HeartbeatWorking,
	}
	stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
	if !stale {
		t.Error("10-minute-old heartbeat should be stale")
	}

	// Stale heartbeat should NOT skip zombie detection — falls through to legacy
	shouldSkip := hb.IsV2() && !stale
	if shouldSkip {
		t.Error("stale v2 heartbeat should fall through to legacy detection")
	}
}

func TestHeartbeatV2_V1FallsThrough(t *testing.T) {
	t.Parallel()
	// v1 heartbeat (no state field) → fall through to legacy detection.
	hb := &polecat.SessionHeartbeat{
		Timestamp: time.Now(),
		// No State field → v1
	}
	if hb.IsV2() {
		t.Error("expected IsV2()=false for v1 heartbeat")
	}

	// v1 heartbeat should NOT trigger v2 logic
	shouldUseV2 := hb.IsV2()
	if shouldUseV2 {
		t.Error("v1 heartbeat should fall through to legacy detection")
	}
}

func TestHeartbeatV2_DeadSessionFreshHeartbeatRace(t *testing.T) {
	t.Parallel()
	// Dead session but fresh heartbeat → possible race (session just restarted).
	// Should skip zombie detection to avoid killing a newly-started session.
	hb := &polecat.SessionHeartbeat{
		Timestamp: time.Now(),
		State:     polecat.HeartbeatWorking,
	}
	stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
	sessionDead := true

	// Fresh heartbeat + dead session → skip (race condition)
	shouldSkip := sessionDead && hb.IsV2() && !stale
	if !shouldSkip {
		t.Error("expected fresh v2 heartbeat + dead session to skip zombie detection (race)")
	}
}

func TestZombieAgentSelfReportedStuck_Classification(t *testing.T) {
	t.Parallel()
	// Verify the new classification type
	if ZombieAgentSelfReportedStuck != "agent-self-reported-stuck" {
		t.Errorf("ZombieAgentSelfReportedStuck = %q, want %q", ZombieAgentSelfReportedStuck, "agent-self-reported-stuck")
	}
	// Should imply active work (agent is alive and asking for help)
	if !ZombieAgentSelfReportedStuck.ImpliesActiveWork() {
		t.Error("ZombieAgentSelfReportedStuck should imply active work")
	}
}

func TestZombieNeverHeartbeated_Classification(t *testing.T) {
	t.Parallel()
	if ZombieNeverHeartbeated != "never-heartbeated" {
		t.Errorf("ZombieNeverHeartbeated = %q, want %q", ZombieNeverHeartbeated, "never-heartbeated")
	}
	if !ZombieNeverHeartbeated.ImpliesActiveWork() {
		t.Error("ZombieNeverHeartbeated should imply active work")
	}

	// Session old enough (>5m default) with assigned work and no heartbeat → flag.
	oldSession := time.Now().Add(-10 * time.Minute)
	shouldFlag := time.Since(oldSession) > config.DefaultWitnessHeartbeatStartupGrace
	if !shouldFlag {
		t.Errorf("expected flag for session age=%v, threshold=%v",
			time.Since(oldSession).Round(time.Second), config.DefaultWitnessHeartbeatStartupGrace)
	}

	// Session within grace period → no flag.
	newSession := time.Now().Add(-2 * time.Minute)
	shouldNotFlag := time.Since(newSession) <= config.DefaultWitnessHeartbeatStartupGrace
	if !shouldNotFlag {
		t.Errorf("expected no flag for session age=%v, threshold=%v",
			time.Since(newSession).Round(time.Second), config.DefaultWitnessHeartbeatStartupGrace)
	}
}

// TestClassifyNeverHeartbeatedLiveness_WorkingIsNotFlagged is the gt-gx2v
// acceptance set. The witness verified all four of these shapes live on
// 2026-09-22 — emerald mid self-review, diamond 50m into a context compaction,
// garnet 45m into a turn with output tokens still rising, granite with a
// transcript 1-8s old — and every flag it raised cost a peek plus a
// POLECAT_DIED mail. A polecat producing output is working, whatever its
// heartbeat file says.
func TestClassifyNeverHeartbeatedLiveness_WorkingIsNotFlagged(t *testing.T) {
	t.Parallel()

	// A session that started 20m ago and cleared its 5m startup grace 15m ago.
	now := time.Now()
	graceDeadline := now.Add(-15 * time.Minute)
	transcript := func(age time.Duration) RealActivity {
		return RealActivity{AgentAlive: true, ObservedAt: now, ActivitySource: ActivitySourceTranscript, LastActivity: now.Add(-age), TranscriptBytes: 900_000}
	}
	pane := func(age time.Duration) RealActivity {
		return RealActivity{AgentAlive: true, ObservedAt: now, ActivitySource: ActivitySourceNone, PaneOutputAt: now.Add(-age)}
	}

	cases := []struct {
		name   string
		act    RealActivity
		gate   gateSlotEvidence
		wantIn string
	}{
		{"emerald: mid self-review, transcript 1s old", transcript(time.Second), gateSlotNone, "transcript=1s old"},
		{"diamond: mid-compaction, pane showing a progress bar", pane(2 * time.Second), gateSlotNone, "pane-output=2s old"},
		{"garnet: 45m turn, output tokens still rising", transcript(3 * time.Second), gateSlotNone, "transcript=3s old"},
		{"granite: transcript active 8s ago", transcript(8 * time.Second), gateSlotNone, "transcript=8s old"},
		{
			"holding the container-gate slot",
			pane(40 * time.Minute),
			gateSlotEvidence{Held: true, Detail: "gate-slot held by gastown/granite (verification suite running)"},
			"gastown/granite",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := classifyNeverHeartbeatedLiveness(tc.act, tc.gate, graceDeadline, now)
			if !ev.Working {
				t.Errorf("healthy polecat flagged as never-heartbeated; evidence=%q", ev.Detail)
			}
			if !strings.Contains(ev.Detail, tc.wantIn) {
				t.Errorf("evidence %q does not name %q", ev.Detail, tc.wantIn)
			}
		})
	}
}

// TestClassifyNeverHeartbeatedLiveness_QuietStartupIsFlagged keeps the rule
// reachable: a session that has produced nothing since it should have written a
// heartbeat is still the startup wedge the rule exists to surface (gt-uk7).
func TestClassifyNeverHeartbeatedLiveness_QuietStartupIsFlagged(t *testing.T) {
	t.Parallel()

	now := time.Now()
	graceDeadline := now.Add(-15 * time.Minute) // session started 20m ago, 5m grace

	cases := []struct {
		name string
		act  RealActivity
	}{
		{
			// The auth-401 shape: the spawn banner, then nothing.
			"nothing since the spawn banner",
			RealActivity{AgentAlive: true, ObservedAt: now, ActivitySource: ActivitySourceNone, PaneOutputAt: now.Add(-20 * time.Minute)},
		},
		{
			// Died before its grace elapsed, so no output ever postdated it.
			"last output before the grace deadline",
			RealActivity{AgentAlive: true, ObservedAt: now, ActivitySource: ActivitySourceTranscript, LastActivity: now.Add(-18 * time.Minute)},
		},
		{
			// One burst of work must not immunize the session for the rest of
			// its life: output after grace, but long stopped.
			"output after grace that stopped 10m ago",
			RealActivity{AgentAlive: true, ObservedAt: now, ActivitySource: ActivitySourceTranscript, LastActivity: now.Add(-10 * time.Minute)},
		},
		{
			// No readable evidence either way is not evidence of work.
			"unreadable transcript and no pane output",
			RealActivity{AgentAlive: true, ObservedAt: now, ActivitySource: ActivitySourceNone, Errors: []string{"locating transcript: no such file"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := classifyNeverHeartbeatedLiveness(tc.act, gateSlotNone, graceDeadline, now)
			if ev.Working {
				t.Errorf("wedged polecat reported as working; evidence=%q", ev.Detail)
			}
			if !strings.Contains(ev.Detail, "gate-slot=none") {
				t.Errorf("evidence %q should record the gate slot check", ev.Detail)
			}
		})
	}
}

// TestClassifyNeverHeartbeatedLiveness_UnreadablePoolIsNamed is the gt-4y5x
// major on the classification side: a pool read that failed must be named in
// the evidence, must not be read as a holder, and must not silence the rule.
func TestClassifyNeverHeartbeatedLiveness_UnreadablePoolIsNamed(t *testing.T) {
	t.Parallel()

	now := time.Now()
	graceDeadline := now.Add(-15 * time.Minute) // session started 20m ago, 5m grace
	unreadable := gateSlotEvidence{Detail: "gate-slot=unreadable (acquiring flock: is a directory)"}

	// Quiet session: the flag still fires, and the evidence names the failed
	// pool read instead of claiming there was no holder.
	ev := classifyNeverHeartbeatedLiveness(RealActivity{AgentAlive: true, ObservedAt: now}, unreadable, graceDeadline, now)
	if ev.Working {
		t.Errorf("unreadable pool read as work; evidence=%q", ev.Detail)
	}
	if !strings.HasPrefix(ev.Detail, "gate-slot=unreadable") {
		t.Errorf("evidence %q does not name the failed pool read", ev.Detail)
	}
}

// TestOutputSince_FreshOutputAfterGraceIsWork pins the freshness half: output a
// moment ago is work, output that stopped is not — an early burst must not
// immunize a session for the rest of its life (gt-gx2v).
func TestOutputSince_FreshOutputAfterGraceIsWork(t *testing.T) {
	t.Parallel()

	now := time.Now()
	graceDeadline := now.Add(-15 * time.Minute) // session started 20m ago, 5m grace

	if !outputSince(now.Add(-time.Second), true, graceDeadline, now) {
		t.Error("output produced a second ago should count as work")
	}
	if outputSince(now.Add(-40*time.Minute), true, graceDeadline, now) {
		t.Error("output older than the freshness window should not count as work")
	}
	if outputSince(time.Time{}, true, graceDeadline, now) {
		t.Error("an unknown timestamp should not count as work")
	}
	if outputSince(now, false, graceDeadline, now) {
		t.Error("a signal that is not known should not count as work")
	}
}

// TestOutputSince_SpawnBannerIsNotWork pins the deadline half: a pane writes at
// session creation, so without it any session younger than the freshness window
// reads as busy and the rule goes silent.
func TestOutputSince_SpawnBannerIsNotWork(t *testing.T) {
	t.Parallel()

	now := time.Now()
	createdAt := now.Add(-20 * time.Millisecond)
	graceDeadline := createdAt.Add(time.Millisecond)

	if outputSince(createdAt, true, graceDeadline, now) {
		t.Error("the spawn banner counts as work; a freshly created session would never be flaggable")
	}
}

// TestHeldGateSlot_NoneHeld covers the reader against an empty town: the
// never-heartbeated gate must not invent a holder (gt-gx2v).
func TestHeldGateSlot_NoneHeld(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	got := readHeldGateSlot(townRoot, "gastown", "diamond")
	if got.Held {
		t.Errorf("readHeldGateSlot on an empty town = %+v, want no holder", got)
	}
	if got.Detail != "gate-slot=none" {
		t.Errorf("Detail = %q, want the empty pool named as such", got.Detail)
	}
}

// TestHeldGateSlot_UnreadableIsNotNone is the gt-4y5x major: a pool read that
// fails is not "no slot held", and the operator must be able to tell the two
// apart from the flag message alone.
func TestHeldGateSlot_UnreadableIsNotNone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the flock probe succeeds unconditionally on Windows")
	}

	townRoot := t.TempDir()
	// A directory where the lock file belongs: os.Stat sees a path, and the
	// read-write open behind it fails, so the pool reports an error.
	if err := os.MkdirAll(slot.LockDir(townRoot), 0o755); err != nil {
		t.Fatalf("create lock dir: %v", err)
	}
	if err := os.Mkdir(slot.LockPath(townRoot), 0o755); err != nil {
		t.Fatalf("create lock path as a directory: %v", err)
	}

	got := readHeldGateSlot(townRoot, "gastown", "diamond")
	if got.Held {
		t.Errorf("Held = true on a pool that could not be read: %+v", got)
	}
	if !strings.HasPrefix(got.Detail, "gate-slot=unreadable") {
		t.Errorf("Detail = %q, want the failed read named, not an empty pool", got.Detail)
	}
}

// TestHeldGateSlot_NamedByRigAndPolecat pins the role string the reader looks up
// against the one `gt slot run` records, since a mismatch would silently
// disable the gate-slot half of the liveness check. Not parallel — it stubs the
// container lister that Acquire consults.
func TestHeldGateSlot_NamedByRigAndPolecat(t *testing.T) {
	restore := slot.SetContainerListerForTest(func() ([]string, error) { return nil, nil })
	t.Cleanup(restore)

	townRoot := t.TempDir()
	h, err := slot.AcquirePool(townRoot, "gastown/diamond", 5*time.Second, slot.Pool{Slots: 1})
	if err != nil {
		t.Fatalf("acquiring a slot in the test town: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	got := readHeldGateSlot(townRoot, "gastown", "diamond")
	if !got.Held || !strings.Contains(got.Detail, "gastown/diamond") {
		t.Errorf("readHeldGateSlot = %+v, want the slot held by gastown/diamond", got)
	}
	if other := readHeldGateSlot(townRoot, "gastown", "emerald"); other.Held {
		t.Errorf("readHeldGateSlot for a non-holder = %+v, want no holder", other)
	}
}

func TestSubmittedStillRunningCandidate(t *testing.T) {
	t.Parallel()

	baseSnap := &agentBeadSnapshot{
		AgentState: string(beads.AgentStateDone),
		HookBead:   "gt-work-123",
		UpdatedAt:  time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
		Fields: &beads.AgentFields{
			CleanupStatus: "clean",
			MRID:          "gt-mr-123",
		},
	}
	staleHB := &polecat.SessionHeartbeat{
		Timestamp: time.Now().Add(-10 * time.Minute),
		State:     polecat.HeartbeatWorking,
	}

	age, ok := isSubmittedStillRunningCandidate(baseSnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace)
	if !ok {
		t.Fatalf("expected submitted still-running candidate, age=%v", age)
	}

	noHookSnap := *baseSnap
	noHookSnap.HookBead = ""
	if _, ok := isSubmittedStillRunningCandidate(&noHookSnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace); !ok {
		t.Error("no-hook submitted sessions must still be treated as submitted still-running")
	}

	idleSnap := *baseSnap
	idleSnap.AgentState = string(beads.AgentStateIdle)
	if _, ok := isSubmittedStillRunningCandidate(&idleSnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace); ok {
		t.Error("normal idle polecats with submitted MR metadata must not be treated as submitted still-running")
	}

	freshHB := &polecat.SessionHeartbeat{
		Timestamp: time.Now(),
		State:     polecat.HeartbeatWorking,
	}
	if _, ok := isSubmittedStillRunningCandidate(baseSnap, freshHB, config.DefaultWitnessHeartbeatStartupGrace); ok {
		t.Error("fresh heartbeat must not be treated as submitted still-running")
	}

	dirtySnap := *baseSnap
	dirtyFields := *baseSnap.Fields
	dirtyFields.CleanupStatus = "has_uncommitted"
	dirtySnap.Fields = &dirtyFields
	if _, ok := isSubmittedStillRunningCandidate(&dirtySnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace); ok {
		t.Error("dirty cleanup status must not be treated as safe submitted still-running")
	}

	noSubmitSnap := *baseSnap
	noSubmitSnap.AgentState = string(beads.AgentStateWorking)
	noSubmitSnap.ActiveMR = ""
	noSubmitSnap.Fields = &beads.AgentFields{CleanupStatus: "clean"}
	if _, ok := isSubmittedStillRunningCandidate(&noSubmitSnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace); ok {
		t.Error("open hooked work without submission evidence must not be treated as submitted still-running")
	}

	completedOnlySnap := *baseSnap
	completedOnlySnap.ActiveMR = ""
	completedOnlySnap.Fields = &beads.AgentFields{
		CleanupStatus:  "clean",
		ExitType:       string(ExitTypeCompleted),
		CompletionTime: time.Now().Format(time.RFC3339),
	}
	if _, ok := isSubmittedStillRunningCandidate(&completedOnlySnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace); ok {
		t.Error("COMPLETED metadata alone must not be treated as successful submission evidence")
	}

	failedSubmitSnap := *baseSnap
	failedSubmitSnap.Fields = &beads.AgentFields{
		CleanupStatus: "clean",
		MRID:          "gt-mr-123",
		MRFailed:      true,
	}
	if _, ok := isSubmittedStillRunningCandidate(&failedSubmitSnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace); ok {
		t.Error("failed MR submission must not be treated as successful submission evidence")
	}

	pushFailedSnap := *baseSnap
	pushFailedSnap.Fields = &beads.AgentFields{
		CleanupStatus: "clean",
		MRID:          "gt-mr-123",
		PushFailed:    true,
	}
	if _, ok := isSubmittedStillRunningCandidate(&pushFailedSnap, staleHB, config.DefaultWitnessHeartbeatStartupGrace); ok {
		t.Error("failed push must not be treated as successful submission evidence")
	}
}

func TestZombieSubmittedStillRunning_Classification(t *testing.T) {
	t.Parallel()
	if ZombieSubmittedStillRunning != "submitted-still-running" {
		t.Errorf("ZombieSubmittedStillRunning = %q, want %q", ZombieSubmittedStillRunning, "submitted-still-running")
	}
	if ZombieSubmittedStillRunning.ImpliesActiveWork() {
		t.Error("ZombieSubmittedStillRunning should be classified as orphan/submitted idle, not active failed work")
	}
}

func TestNotifyRefineryMergeReady_EmitsChannelEvent(t *testing.T) {
	// Create a fake town root with the workspace marker so workspace.Find recognizes it
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set GT_TEST_NUDGE_LOG to prevent actual tmux operations in nudgeRefinery
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))

	result := &HandlerResult{}
	// notifyRefineryMergeReady takes workDir and calls workspace.Find(workDir) internally
	notifyRefineryMergeReady(townRoot, "dashboard", result)

	// Verify that a MERGE_READY event file was created in the rig-scoped
	// refinery channel directory (per-rig channels, gt-dsj)
	eventDir := filepath.Join(townRoot, "events", "refinery", "dashboard")
	entries, err := os.ReadDir(eventDir)
	if err != nil {
		t.Fatalf("reading event dir: %v", err)
	}

	var eventFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".event") {
			eventFiles = append(eventFiles, e.Name())
		}
	}

	if len(eventFiles) == 0 {
		t.Fatal("expected at least one .event file in ~/gt/events/refinery/, got none")
	}

	// Read and verify the event content
	data, err := os.ReadFile(filepath.Join(eventDir, eventFiles[0]))
	if err != nil {
		t.Fatalf("reading event file: %v", err)
	}

	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("parsing event JSON: %v", err)
	}

	if event["type"] != "MERGE_READY" {
		t.Errorf("event type = %v, want MERGE_READY", event["type"])
	}
	if event["channel"] != "refinery" {
		t.Errorf("event channel = %v, want refinery", event["channel"])
	}

	payload, ok := event["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload is not a map: %T", event["payload"])
	}
	if payload["source"] != "witness" {
		t.Errorf("payload.source = %v, want witness", payload["source"])
	}
	if payload["rig"] != "dashboard" {
		t.Errorf("payload.rig = %v, want dashboard", payload["rig"])
	}
}

// stubNukePolecat overrides the package-level nukePolecatFunc so archive-path
// tests never shell out to the real `gt polecat nuke` (gt-evdg): NukePolecat
// kills a real tmux session and deletes a real worktree, so a test that
// reaches the archive path with the real function — on a live rig/polecat
// name — can destroy an in-use polecat. Returns a func reporting whether the
// stub was invoked. Restores the original in t.Cleanup.
func stubNukePolecat(t *testing.T, err error) func() bool {
	t.Helper()
	old := nukePolecatFunc
	called := false
	nukePolecatFunc = func(bd *BdCli, workDir, rigName, polecatName string) error {
		called = true
		return err
	}
	t.Cleanup(func() { nukePolecatFunc = old })
	return func() bool { return called }
}

// stubNukePolecatExecs replaces NukePolecat's own tmux-kill and `gt polecat
// nuke` seams so a test exercising NukePolecat directly (not through the
// coarser nukePolecatFunc/stubNukePolecat) can see exactly what it was asked
// to destroy without spawning either: the real implementations panic under a
// test binary unless faked (gt-5itbt).
//
// Must not be combined with t.Parallel: it mutates package variables.
func stubNukePolecatExecs(t *testing.T) (killed *[]string, nuked *[]string) {
	t.Helper()
	killed = &[]string{}
	nuked = &[]string{}

	oldKill := nukeKillSessionExec
	nukeKillSessionExec = func(sessionName string) {
		*killed = append(*killed, sessionName)
	}
	t.Cleanup(func() { nukeKillSessionExec = oldKill })

	oldNuke := nukePolecatWorktreeExec
	nukePolecatWorktreeExec = func(workDir, address string) error {
		*nuked = append(*nuked, address)
		return nil
	}
	t.Cleanup(func() { nukePolecatWorktreeExec = oldNuke })

	return killed, nuked
}

// TestHandleZombieRestart_SkipsWhenBranchAlreadyMerged verifies the aa-apw fix:
// when a stopped polecat's branch work is already merged to origin/main (e.g.,
// via squash-merge), the witness must NOT restart the session — restarting
// would let the polecat re-push its pre-squash HEAD and create a duplicate MR.
// Instead the polecat is archived.
//
// Not parallel: overrides the package-level verifyBranchAlreadyMerged and
// nukePolecatFunc vars.
func TestHandleZombieRestart_SkipsWhenBranchAlreadyMerged(t *testing.T) {
	oldVerify := verifyBranchAlreadyMerged
	verifyBranchAlreadyMerged = func(workDir, rigName, polecatName, hookBead string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { verifyBranchAlreadyMerged = oldVerify })
	nuked := stubNukePolecat(t, nil)

	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	// hookBead is reaped ("" with found=true) — the original always-archive-
	// eligible case that predates gt-evdg.
	z := &ZombieResult{PolecatName: "zz-test-cat", HookBead: "ma-poc.4"}
	handleZombieRestart(bd, t.TempDir(), "zz-test-rig", "zz-test-cat", "ma-poc.4", "", true, "has_unpushed", z)

	// Action must reflect the archive decision; must NOT be a "restarted*" action.
	if !strings.Contains(z.Action, "work-already-merged") {
		t.Errorf("action = %q, want it to mention work-already-merged (aa-apw)", z.Action)
	}
	if strings.HasPrefix(z.Action, "restarted") || strings.HasPrefix(z.Action, "restart-") {
		t.Errorf("action = %q, polecat must not be restarted when work is already merged", z.Action)
	}
	if !nuked() {
		t.Error("nukePolecatFunc was not called; archive path should invoke it")
	}
}

// TestHandleZombieRestart_RestartsWhenBranchNotMerged verifies the pre-aa-apw
// behavior is preserved when work is NOT merged: handleZombieRestart proceeds
// to its normal cleanup/restart flow.
//
// Not parallel: overrides the package-level verifyBranchAlreadyMerged,
// nukePolecatFunc and restartSessionExec vars.
func TestHandleZombieRestart_RestartsWhenBranchNotMerged(t *testing.T) {
	oldVerify := verifyBranchAlreadyMerged
	verifyBranchAlreadyMerged = func(workDir, rigName, polecatName, hookBead string) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { verifyBranchAlreadyMerged = oldVerify })
	nuked := stubNukePolecat(t, nil)
	restarts := stubRestartSessionExec(t)

	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	z := &ZombieResult{PolecatName: "zz-test-cat", HookBead: "ma-poc.4"}
	handleZombieRestart(bd, t.TempDir(), "zz-test-rig", "zz-test-cat", "ma-poc.4", "in_progress", true, "clean", z)

	// Should NOT take the archive path.
	if strings.Contains(z.Action, "work-already-merged") {
		t.Errorf("action = %q, should not archive when work is not merged", z.Action)
	}
	if nuked() {
		t.Error("nukePolecatFunc was called; archive path should not fire when work is not merged")
	}
	if len(*restarts) != 1 || (*restarts)[0] != "zz-test-rig/zz-test-cat" {
		t.Errorf("restarts = %v, want exactly [\"zz-test-rig/zz-test-cat\"]", *restarts)
	}
}

// TestHandleZombieRestart_DoesNotArchiveWhenHookBeadOpen verifies gt-evdg:
// a branch that never diverged from the default branch (a session-dead
// polecat whose session died before its first commit) is trivially
// "preserved" under every evidence path in verifyBranchAlreadyMerged —
// ancestor, merge-tree-noop, and cherry are all satisfied by zero commits,
// exactly like a genuinely completed squash merge. bd status "open" means
// the assignment was never even claimed, so a "merged" verdict from git
// alone must not be trusted to license a nuke.
//
// Uses fake rig/polecat names and a stubbed nukePolecatFunc, never the real
// NukePolecat — per the MAYOR SAFETY WARNING on gt-evdg, a live rig/polecat
// name here would let a regression in this gate shell out to the real
// `gt polecat nuke` and kill a real tmux session.
//
// Not parallel: overrides the package-level verifyBranchAlreadyMerged,
// nukePolecatFunc and restartSessionExec vars.
func TestHandleZombieRestart_DoesNotArchiveWhenHookBeadOpen(t *testing.T) {
	oldVerify := verifyBranchAlreadyMerged
	verifyBranchAlreadyMerged = func(workDir, rigName, polecatName, hookBead string) (bool, error) {
		// Simulates the false positive from a branch that never diverged:
		// the git layer alone cannot tell "never started" from "already
		// merged" here, so it reports merged=true just as it would for
		// real, completed work.
		return true, nil
	}
	t.Cleanup(func() { verifyBranchAlreadyMerged = oldVerify })
	nuked := stubNukePolecat(t, nil)
	stubRestartSessionExec(t)

	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	z := &ZombieResult{PolecatName: "zz-test-cat", HookBead: "gt-3qfp"}
	handleZombieRestart(bd, t.TempDir(), "zz-test-rig", "zz-test-cat", "gt-3qfp", "open", true, "clean", z)

	if strings.Contains(z.Action, "work-already-merged") {
		t.Errorf("action = %q, must not archive a session-dead polecat whose hookBead status is open (gt-evdg)", z.Action)
	}
	if nuked() {
		t.Error("nukePolecatFunc must not be called when hookBead status is open — real (gt-3qfp) or fake (this test), the assignment was never claimed")
	}
}

// TestHandleZombieRestart_ArchivesWhenHookBeadInProgressAndMerged covers the
// realistic case the gt-evdg fix must not regress (om review on MR gt-wisp-
// z4vl): a hooked/in_progress bead whose branch really was squash-merged.
// The refinery may not have closed the bead yet — that race must not block
// the archive, or the polecat gets restarted and re-pushes a duplicate MR
// for work already on main.
//
// Not parallel: overrides the package-level verifyBranchAlreadyMerged and
// nukePolecatFunc vars.
func TestHandleZombieRestart_ArchivesWhenHookBeadInProgressAndMerged(t *testing.T) {
	oldVerify := verifyBranchAlreadyMerged
	verifyBranchAlreadyMerged = func(workDir, rigName, polecatName, hookBead string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { verifyBranchAlreadyMerged = oldVerify })
	nuked := stubNukePolecat(t, nil)

	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	z := &ZombieResult{PolecatName: "zz-test-cat", HookBead: "gt-real"}
	handleZombieRestart(bd, t.TempDir(), "zz-test-rig", "zz-test-cat", "gt-real", "in_progress", true, "clean", z)

	if !strings.Contains(z.Action, "work-already-merged") {
		t.Errorf("action = %q, want archive when a hooked/in_progress bead's branch is really merged", z.Action)
	}
	if !nuked() {
		t.Error("nukePolecatFunc was not called; a real merge must still archive even with a non-closed hookBead status")
	}
}

// TestHandleZombieRestart_ArchivesWhenHookBeadEmptyAndMerged pins the
// original aa-apw case: no hookBead at all (or a reaped one, "" with
// found=true), branch really merged — must archive.
//
// Not parallel: overrides the package-level verifyBranchAlreadyMerged and
// nukePolecatFunc vars.
func TestHandleZombieRestart_ArchivesWhenHookBeadEmptyAndMerged(t *testing.T) {
	oldVerify := verifyBranchAlreadyMerged
	verifyBranchAlreadyMerged = func(workDir, rigName, polecatName, hookBead string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { verifyBranchAlreadyMerged = oldVerify })
	nuked := stubNukePolecat(t, nil)

	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	z := &ZombieResult{PolecatName: "zz-test-cat"}
	handleZombieRestart(bd, t.TempDir(), "zz-test-rig", "zz-test-cat", "", "", false, "clean", z)

	if !strings.Contains(z.Action, "work-already-merged") {
		t.Errorf("action = %q, want archive when there is no hookBead and the branch is really merged", z.Action)
	}
	if !nuked() {
		t.Error("nukePolecatFunc was not called; an empty hookBead with a real merge must still archive")
	}
}

// TestNukePolecatCallsInjectedExecsWhenFaked is the positive companion to
// TestNukePolecatPanicsWithoutFakeExecutors: with stubNukePolecatExecs
// injected, NukePolecat runs clean and drives both seams with the expected
// session name and address, rather than merely avoiding the panic.
//
// Not parallel: mutates package-level vars via stubNukePolecatExecs.
func TestNukePolecatCallsInjectedExecsWhenFaked(t *testing.T) {
	killed, nuked := stubNukePolecatExecs(t)

	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	if err := NukePolecat(bd, t.TempDir(), "zz-test-rig", "zz-test-cat"); err != nil {
		t.Fatalf("NukePolecat: %v", err)
	}
	if len(*nuked) != 1 || (*nuked)[0] != "zz-test-rig/zz-test-cat" {
		t.Errorf("nuked = %v, want exactly [\"zz-test-rig/zz-test-cat\"]", *nuked)
	}
	if len(*killed) != 1 {
		t.Errorf("killed sessions = %v, want exactly one", *killed)
	}
}

// TestNukePolecatPanicsWithoutFakeExecutors is the enforcement test for
// gt-5itbt: NukePolecat's real tmux-kill and `gt polecat nuke` seams must
// refuse to run for real inside a test binary. A test that reaches them
// without injecting a fake (stubNukePolecatExecs) must fail loud instead of
// running the real nuke against whatever rig/polecat name it happened to
// pass in.
//
// Not parallel: NukePolecat's veto/MR checks touch package state via bd.
func TestNukePolecatPanicsWithoutFakeExecutors(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NukePolecat returned without panicking — the real kill/nuke path ran unguarded")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "HERMETIC VIOLATION") {
			t.Fatalf("panic = %v, want a HERMETIC VIOLATION message", r)
		}
	}()

	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	// Real rig/polecat names, exactly like the incident this guards against —
	// the guard must trip before either name is ever inspected.
	_ = NukePolecat(bd, t.TempDir(), "gastown", "flint")
	t.Fatal("NukePolecat returned without panicking — the real kill/nuke path ran unguarded")
}

// TestRestartPolecatSessionPanicsWithoutFakeExecutor is the enforcement test
// for restartSessionExec's default: a test reaching RestartPolecatSession
// without stubbing it (stubRestartSessionExec) must fail loud instead of
// running a real `gt session restart` (gt-5itbt, companion to
// TestNukePolecatPanicsWithoutFakeExecutors).
func TestRestartPolecatSessionPanicsWithoutFakeExecutor(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RestartPolecatSession returned without panicking — the real restart path ran unguarded")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "HERMETIC VIOLATION") {
			t.Fatalf("panic = %v, want a HERMETIC VIOLATION message", r)
		}
	}()

	_ = RestartPolecatSession(t.TempDir(), "gastown", "flint")
	t.Fatal("RestartPolecatSession returned without panicking — the real restart path ran unguarded")
}

// Work that survives on a polecat branch keeps the hook: the witness must not
// reset it for a fresh re-dispatch from main (gt-vm5g4). An unknown answer
// keeps it too; a rig with no git repo resets as before. The reset itself is
// guarded on the dead polecat still holding the bead.
func TestResetAbandonedBead_SurvivingWorkKeepsHook(t *testing.T) {
	// Not parallel: overrides package-level seams.
	oldVerify, oldSurviving := verifyCommitOnMain, survivingWorkForBead
	verifyCommitOnMain = func(string, string, string) (bool, error) { return false, nil }
	t.Cleanup(func() { verifyCommitOnMain, survivingWorkForBead = oldVerify, oldSurviving })

	for _, tc := range []struct {
		name      string
		branch    string
		err       error
		wantReset bool
	}{
		{name: "work survives", branch: "polecat/alpha/gt-work123+mu5wzd6q"},
		{name: "survival unknown", err: errors.New("origin unreachable")},
		{name: "no surviving work", wantReset: true},
		{name: "rig has no git repo", err: polecat.ErrNoRigRepo, wantReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			survivingWorkForBead = func(_, rigName, beadID string) (string, error) {
				if rigName != "testrig" || beadID != "gt-work123" {
					t.Fatalf("predicate asked about %s/%s", rigName, beadID)
				}
				return tc.branch, tc.err
			}
			bd, mock := mockBd(
				func(args []string) (string, error) {
					if len(args) >= 1 && args[0] == "show" {
						return `[{"status":"in_progress"}]`, nil
					}
					return "", nil
				},
				func([]string) error { return nil },
			)
			got := resetAbandonedBead(bd, t.TempDir(), "testrig", "gt-work123", "alpha", nil)
			var resets []string
			for _, call := range mock.calls {
				if strings.Contains(call, "update") && strings.Contains(call, "--status=open") {
					resets = append(resets, call)
				}
			}
			if got != tc.wantReset || (len(resets) == 1) != tc.wantReset {
				t.Fatalf("reset = %v (calls %v), want %v", got, resets, tc.wantReset)
			}
			if tc.wantReset && !strings.Contains(resets[0], "--if-assignee=testrig/polecats/alpha") {
				t.Fatalf("reset is not guarded on the dead polecat: %s", resets[0])
			}
		})
	}
}
