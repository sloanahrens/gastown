package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery"
)

// capturePostMergeEscalations swaps postMergeEscalate for the test's
// duration. Tests that use it swap a package var, so they must not run with
// t.Parallel.
func capturePostMergeEscalations(t *testing.T) *[]string {
	t.Helper()
	esc, _ := capturePostMergeAlerts(t)
	return esc
}

// capturePostMergeAlerts swaps both alert seams, the escalate and the clear,
// so no test ever execs a real gt. It returns the escalations and the rigs
// whose escalation was cleared.
func capturePostMergeAlerts(t *testing.T) (*[]string, *[]string) {
	t.Helper()
	var got, clears []string
	origEsc, origClear := postMergeEscalate, postMergeClear
	postMergeEscalate = func(rigName, msg string) { got = append(got, rigName+": "+msg) }
	postMergeClear = func(rigName string) { clears = append(clears, rigName) }
	t.Cleanup(func() { postMergeEscalate, postMergeClear = origEsc, origClear })
	return &got, &clears
}

func TestRunPostMergeCommand_SuccessClearsEscalation(t *testing.T) {
	esc, clears := capturePostMergeAlerts(t)
	runPostMergeCommand(postMergeCommandParams{
		RigName: "gastown",
		WorkDir: t.TempDir(),
		Timeout: 10 * time.Second,
		Output:  io.Discard,
		Command: "true",
	})
	if len(*esc) != 0 {
		t.Errorf("success escalated: %v", *esc)
	}
	if len(*clears) != 1 || (*clears)[0] != "gastown" {
		t.Fatalf("clears = %v, want exactly [gastown]", *clears)
	}
}

func TestRunPostMergeCommand_FailureEscalatesWithoutClear(t *testing.T) {
	esc, clears := capturePostMergeAlerts(t)
	runPostMergeCommand(postMergeCommandParams{
		RigName: "gastown",
		WorkDir: t.TempDir(),
		Timeout: 10 * time.Second,
		Output:  io.Discard,
		Command: "exit 3",
	})
	if len(*esc) != 1 {
		t.Fatalf("escalations = %v, want exactly 1", *esc)
	}
	if len(*clears) != 0 {
		t.Fatalf("failure cleared the escalation: %v", *clears)
	}
}

func TestRunPostMergeCommand_EmptyCommandNeitherEscalatesNorClears(t *testing.T) {
	esc, clears := capturePostMergeAlerts(t)
	runPostMergeCommand(postMergeCommandParams{RigName: "gastown", WorkDir: t.TempDir(), Output: io.Discard})
	if len(*esc) != 0 || len(*clears) != 0 {
		t.Fatalf("empty command: escalations=%v clears=%v, want none", *esc, *clears)
	}
}

func TestPostMergeFingerprintSharedByEscalateAndClear(t *testing.T) {
	if got := postMergeFingerprint("gastown"); got != "post-merge-command:gastown" {
		t.Fatalf("fingerprint = %q", got)
	}
}

func capturePostMergeCommandCalls(t *testing.T) *[]postMergeCommandParams {
	t.Helper()
	var calls []postMergeCommandParams
	orig := postMergeCommandFn
	postMergeCommandFn = func(p postMergeCommandParams) { calls = append(calls, p) }
	t.Cleanup(func() { postMergeCommandFn = orig })
	return &calls
}

func TestRunPostMergeCommand_EmptyCommandIsNoop(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	var out bytes.Buffer
	runPostMergeCommand(postMergeCommandParams{RigName: "gastown", WorkDir: t.TempDir(), Output: &out})
	if out.Len() != 0 {
		t.Errorf("empty command wrote output: %q", out.String())
	}
	if len(*esc) != 0 {
		t.Errorf("empty command escalated: %v", *esc)
	}
}

func TestRunPostMergeCommand_PassesEnvAndWorkDir(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	dir := t.TempDir()
	runPostMergeCommand(postMergeCommandParams{
		RigName:   "gastown",
		TownRoot:  "/town",
		WorkDir:   dir,
		MergedSHA: "abc123",
		MRIDs:     []string{"gt-mr1", "gt-mr2"},
		Timeout:   10 * time.Second,
		Output:    io.Discard,
		Command:   `printf '%s|%s|%s|%s|%s\n' "$GT_MERGED_SHA" "$GT_RIG" "$GT_TOWN_ROOT" "$GT_MR_IDS" "$(pwd -P)" > env.txt`,
	})
	if len(*esc) != 0 {
		t.Fatalf("successful command escalated: %v", *esc)
	}
	data, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatalf("command did not run in WorkDir: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "abc123|gastown|/town|gt-mr1,gt-mr2|" + resolved
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("env = %q, want %q", got, want)
	}
}

func TestRunPostMergeCommand_FailureEscalatesAndReturns(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	var out bytes.Buffer
	runPostMergeCommand(postMergeCommandParams{
		RigName:   "gastown",
		WorkDir:   t.TempDir(),
		MergedSHA: "abc123",
		Timeout:   10 * time.Second,
		Output:    &out,
		Command:   "echo building; exit 7",
	})
	if !strings.Contains(out.String(), "building") {
		t.Errorf("command output not streamed: %q", out.String())
	}
	if len(*esc) != 1 {
		t.Fatalf("escalations = %v, want exactly 1", *esc)
	}
	if !strings.Contains((*esc)[0], "exit status 7") || !strings.HasPrefix((*esc)[0], "gastown: ") {
		t.Errorf("escalation = %q, want the rig and the exit status", (*esc)[0])
	}
}

func TestRunPostMergeCommand_TimeoutKillsProcessGroup(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	dir := t.TempDir()
	start := time.Now()
	runPostMergeCommand(postMergeCommandParams{
		RigName: "gastown",
		WorkDir: dir,
		Timeout: time.Second,
		Output:  io.Discard,
		Command: `sleep 30 & echo $! > child.pid; wait`,
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runner took %v; the 1s timeout did not fire", elapsed)
	}
	if len(*esc) != 1 || !strings.Contains((*esc)[0], "timed out") {
		t.Fatalf("escalations = %v, want one 'timed out'", *esc)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("reading child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing child pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived the timeout: the process group was not killed", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPostMergeCommandFor_UnconfiguredIsOff(t *testing.T) {
	t.Parallel()
	if _, ok := postMergeCommandFor("/town", "gastown", "/town/gastown", nil, "abc", "main", nil, io.Discard); ok {
		t.Error("nil config: ok = true, want false")
	}
	if _, ok := postMergeCommandFor("/town", "gastown", "/town/gastown", &config.MergeQueueConfig{}, "abc", "main", nil, io.Discard); ok {
		t.Error("empty post_merge_command: ok = true, want false")
	}
}

func TestPostMergeCommandFor_UsesRefineryWorktreeAndMergeCommit(t *testing.T) {
	t.Parallel()
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh", PostMergeTimeout: "90s"}
	p, ok := postMergeCommandFor("/town", "gastown", "/town/gastown", mq, "abc123", "main", []string{"gt-mr1"}, io.Discard)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if p.WorkDir != filepath.Join("/town/gastown", "refinery", "rig") {
		t.Errorf("WorkDir = %q, want <rig>/refinery/rig", p.WorkDir)
	}
	if p.MergedSHA != "abc123" || p.Command != "scripts/install-after-merge.sh" || p.Timeout != 90*time.Second {
		t.Errorf("params = %+v", p)
	}
	if p.RigName != "gastown" || p.TownRoot != "/town" || len(p.MRIDs) != 1 || p.MRIDs[0] != "gt-mr1" {
		t.Errorf("params = %+v", p)
	}
}

func TestPostMergeCommandFor_FallsBackToOriginTarget(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	work := filepath.Join(rigPath, "refinery", "rig")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", work, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitT("init", "-q")
	gitT("commit", "-q", "--allow-empty", "-m", "base")
	head := gitT("rev-parse", "HEAD")
	gitT("update-ref", "refs/remotes/origin/main", head)

	mq := &config.MergeQueueConfig{PostMergeCommand: "true"}
	p, ok := postMergeCommandFor("/town", "gastown", rigPath, mq, "", "main", nil, io.Discard)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if p.MergedSHA != head {
		t.Errorf("MergedSHA = %q, want origin/main %q", p.MergedSHA, head)
	}
}

func TestRunMRPostMergeCommand_CallsOnceWithMRFields(t *testing.T) {
	calls := capturePostMergeCommandCalls(t)
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh"}

	runMRPostMergeCommand("/town", "gastown", "/town/gastown", mq, nil, io.Discard)
	if len(*calls) != 0 {
		t.Fatalf("nil MR: calls = %d, want 0", len(*calls))
	}

	mr := &refinery.MergeRequest{ID: "gt-mr1", TargetBranch: "main", MergeCommit: "abc123"}
	runMRPostMergeCommand("/town", "gastown", "/town/gastown", mq, mr, io.Discard)
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.MergedSHA != "abc123" || len(got.MRIDs) != 1 || got.MRIDs[0] != "gt-mr1" {
		t.Errorf("params = %+v", got)
	}

	runMRPostMergeCommand("/town", "gastown", "/town/gastown", &config.MergeQueueConfig{}, mr, io.Discard)
	if len(*calls) != 1 {
		t.Errorf("unconfigured rig: calls = %d, want still 1", len(*calls))
	}
}

func TestRunBatchPostMergeCommand_OncePerLandedBatch(t *testing.T) {
	calls := capturePostMergeCommandCalls(t)
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh"}
	result := &refinery.BatchResult{
		Merged:      []*refinery.MRInfo{{ID: "gt-a"}, {ID: "gt-b"}, {ID: "gt-c"}},
		MergeCommit: "tip123",
		Error:       errors.New("cleanup failed for gt-c"),
	}

	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", mq, result, "main", io.Discard)

	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want exactly 1 per batch", len(*calls))
	}
	got := (*calls)[0]
	if got.MergedSHA != "tip123" {
		t.Errorf("MergedSHA = %q, want the batch tip", got.MergedSHA)
	}
	if strings.Join(got.MRIDs, ",") != "gt-a,gt-b,gt-c" {
		t.Errorf("MRIDs = %v, want every merged member", got.MRIDs)
	}
}

func TestRunBatchPostMergeCommand_SkipsWhenNothingLanded(t *testing.T) {
	calls := capturePostMergeCommandCalls(t)
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh"}

	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", mq, nil, "main", io.Discard)
	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", mq, &refinery.BatchResult{Error: errors.New("gate red")}, "main", io.Discard)
	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", &config.MergeQueueConfig{}, &refinery.BatchResult{MergeCommit: "tip123"}, "main", io.Discard)

	if len(*calls) != 0 {
		t.Fatalf("calls = %d, want 0 (nothing landed, or no command configured)", len(*calls))
	}
}
