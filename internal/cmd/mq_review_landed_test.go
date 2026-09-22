package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// mqReviewFlagNames are the flags gt mq review binds to package-level vars.
var mqReviewFlagNames = []string{
	"rehearsed", "json", "force", "attempt", "timeout", "reroll", "landed", "target", "rig",
}

type savedFlagState struct {
	value   string
	changed bool
}

// saveReviewFlags snapshots gt mq review's flags and returns a func that puts
// them back. The flags are package-level vars and cobra keeps their Changed
// bits for the life of the process, so two invocations in one test binary
// would otherwise share state: the second runs with the first's --landed (and
// a --timeout that still reads as explicitly set), and a test then passes or
// fails on the order it happened to run in.
func saveReviewFlags(t *testing.T) func() {
	t.Helper()
	saved := make(map[string]savedFlagState, len(mqReviewFlagNames))
	for _, name := range mqReviewFlagNames {
		f := mqReviewCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("gt mq review has no --%s flag", name)
		}
		saved[name] = savedFlagState{value: f.Value.String(), changed: f.Changed}
	}
	return func() {
		for name, s := range saved {
			f := mqReviewCmd.Flags().Lookup(name)
			if err := f.Value.Set(s.value); err != nil {
				t.Errorf("restoring --%s: %v", name, err)
			}
			f.Changed = s.changed
		}
	}
}

// runReviewArgs drives the real gt mq review command tree — rootCmd, cobra's
// own arg parsing, runMQReview's exit-code paths — and returns its exit code.
// The gate is faked for the duration of the call so the test never invokes om.
func runReviewArgs(t *testing.T, rigDir string, gate func(req editorial.ReviewRequest) editorial.ReviewResult, args ...string) (int, *editorial.ReviewRequest) {
	t.Helper()
	t.Chdir(rigDir)

	restoreFlags := saveReviewFlags(t)
	defer restoreFlags()

	oldGate := runEditorialReview
	var captured *editorial.ReviewRequest
	runEditorialReview = func(req editorial.ReviewRequest, _ editorial.Deps) editorial.ReviewResult {
		captured = &req
		return gate(req)
	}
	defer func() { runEditorialReview = oldGate }()

	oldArgs := os.Args
	os.Args = append([]string{"gt"}, args...)
	defer func() { os.Args = oldArgs }()

	return CodeOf(t, rootCmd), captured
}

// CodeOf runs a cobra command and maps its error onto the CLI's exit-code
// contract the way Execute does: a SilentExitError carries its code, any other
// error is exit 1.
func CodeOf(t *testing.T, cmd *cobra.Command) int {
	t.Helper()
	err := cmd.Execute()
	if err == nil {
		return 0
	}
	if code, ok := IsSilentExit(err); ok {
		return code
	}
	t.Errorf("command error: %v", err)
	return 1
}

// approveGate stands in for a gate run that approves. It answers with a Note
// because a real 0/1 verdict always carries one — printMQReviewResult reads it
// on that path — so a fake that omits it would be testing a state production
// cannot reach.
func approveGate(t *testing.T) func(editorial.ReviewRequest) editorial.ReviewResult {
	t.Helper()
	return func(editorial.ReviewRequest) editorial.ReviewResult {
		return editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Verdict: "approve", Score: 0.87}}
	}
}

// testRigRoot lays out a minimal Gas Town town in a temp dir: a workspace
// marker, one rig with the editorial gate required, a polecat subdir to stand
// in for the caller's cwd, and the rig's refinery clone with a landed commit
// on its remote. It returns the caller cwd (which the caller chdirs into), the
// repo dir, and the rig dir.
//
// No beads store is needed: a landed review mints no MR bead and the gate is
// faked, so the command never reads or writes beads.
func testRigRoot(t *testing.T, defaultBranch string) (cwd, repoDir, rigDir string) {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The rig registry getRig consults (internal/cmd/rig_helpers.go).
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"),
		[]byte(`{"version":1,"rigs":{"gastown":{"git_url":"file:///nonexistent","beads":{"repo":"local","prefix":"gt"}}}}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rigDir = filepath.Join(town, "gastown")
	repoDir = filepath.Join(rigDir, "refinery", "rig")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	// cwd names the rig: findCurrentRig reads the first component of the path
	// from the town root, so no GT_RIG is needed.
	cwd = filepath.Join(rigDir, "polecats", "shale")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}
	// The rig-root config.json ResolveMergeQueueConfig reads as its floor
	// tier, with the editorial gate required so --landed reviews run without
	// --force.
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"),
		[]byte(`{"type":"rig","name":"gastown","git_url":"file:///nonexistent","merge_queue":{"editorial":{"required":true}}}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(repoDir, "init", "--initial-branch", defaultBranch)
	run(repoDir, "commit", "--allow-empty", "-m", "base")
	bare := t.TempDir()
	run(bare, "init", "--bare", "--initial-branch", defaultBranch)
	run(repoDir, "remote", "add", "origin", bare)
	run(repoDir, "push", "-u", "origin", defaultBranch)
	run(repoDir, "commit", "--allow-empty", "-m", "mid")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(repoDir, "add", ".")
	run(repoDir, "commit", "-m", "landed work")
	run(repoDir, "push", "origin", defaultBranch)
	// An unpushed branch tip. It descends from the landed tip, so it is a
	// commit origin/<target> cannot contain: reachability is what refuses it.
	run(repoDir, "checkout", "-b", "unpushed-branch")
	run(repoDir, "commit", "--allow-empty", "-m", "unpushed")
	run(repoDir, "checkout", defaultBranch)

	t.Chdir(cwd)
	return cwd, repoDir, rigDir
}

func revForCmd(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// noGate fails the test if the gate runs: a refusal must stop before om.
func noGate(t *testing.T) func(editorial.ReviewRequest) editorial.ReviewResult {
	t.Helper()
	return func(editorial.ReviewRequest) editorial.ReviewResult {
		t.Error("the gate must not run")
		return editorial.ReviewResult{}
	}
}

// TestMQReviewLanded_NotLandedRefusedExitsTwo covers a commit the target does
// not contain — never landed, or reverted since.
func TestMQReviewLanded_NotLandedRefusedExitsTwo(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	unpushed := revForCmd(t, repoDir, "unpushed-branch")

	code, captured := runReviewArgs(t, rigDir, noGate(t), "mq", "review", "--landed", unpushed)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (a refused range is a config error, never request_changes)", code)
	}
	if captured != nil {
		t.Error("the gate ran despite a refused range")
	}
}

// TestMQReviewLanded_RefusalsOtherThanReachabilityExitTwo pins the other ways
// ResolveLandedRange refuses. Their messages name neither "not on <target>"
// nor "not reachable from", so a caller that recognized refusals by matching
// those two substrings let them through with a zero-valued range and ran the
// gate on a diff that did not exist — fail-open, on the path where failing
// open is exactly wrong.
func TestMQReviewLanded_RefusalsOtherThanReachabilityExitTwo(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	root := revForCmd(t, repoDir, "HEAD~2") // the fixture's root commit

	for _, tc := range []struct {
		name string
		sha  string
	}{
		{"root commit", root},
		{"unknown sha", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, captured := runReviewArgs(t, rigDir, noGate(t), "mq", "review", "--landed", tc.sha)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if captured != nil {
				t.Error("the gate ran despite a refused range")
			}
		})
	}
}

func TestMQReviewLanded_ApproveExitsZeroAndResolvesRange(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	tip := revForCmd(t, repoDir, "HEAD")

	var captured *editorial.ReviewRequest
	code, _ := runReviewArgs(t, rigDir,
		func(req editorial.ReviewRequest) editorial.ReviewResult {
			captured = &req
			return approveGate(t)(req)
		},
		"mq", "review", "--landed", tip)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (approve)", code)
	}
	if captured == nil {
		t.Fatal("the gate never ran")
	}
	if captured.Landed == nil {
		t.Fatal("request.Landed is nil; --landed must resolve a range")
	}
	if captured.Landed.Commit != tip {
		t.Errorf("Landed.Commit = %s, want %s", captured.Landed.Commit, tip)
	}
	// A single-parent landing: the commit's own diff is the range, and the
	// patch-id is the one the coverage check recomputes from the landed commit.
	parent := captured.Landed.Parent
	if captured.Landed.Base != parent || captured.Landed.Head != tip {
		t.Errorf("Landed Base/Head = %s/%s, want %s/%s (the commit's own diff)",
			captured.Landed.Base, captured.Landed.Head, parent, tip)
	}
	if captured.Landed.PatchID == "" {
		t.Error("Landed.PatchID is empty; the note would be keyed on nothing")
	}
	if captured.RehearsedHead != "" {
		t.Errorf("RehearsedHead = %q, want empty (a landed review rehearses nothing)", captured.RehearsedHead)
	}
	if captured.Rig != "gastown" || captured.Target != "main" {
		t.Errorf("Rig/Target = %q/%q, want gastown/main", captured.Rig, captured.Target)
	}
	// No MR bead is minted for a retro review: the gate script reads an absent
	// --mr as "do not route this verdict to a bead", and an open bead labeled
	// gt:merge-request would read as a queueable merge request to every
	// consumer that selects on that label.
	if captured.MRID != "" {
		t.Errorf("MRID = %q, want empty (a landed review mints no MR bead)", captured.MRID)
	}
}

func TestMQReviewLanded_RequestChangesExitsOne(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	tip := revForCmd(t, repoDir, "HEAD")

	code, _ := runReviewArgs(t, rigDir,
		func(editorial.ReviewRequest) editorial.ReviewResult {
			return editorial.ReviewResult{Exit: 1, Note: &editorial.Note{Verdict: "request_changes", Score: 0.38}}
		},
		"mq", "review", "--landed", tip)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (request_changes)", code)
	}
}

func TestMQReviewLanded_PositionalMRID(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	tip := revForCmd(t, repoDir, "HEAD")

	var captured *editorial.ReviewRequest
	code, _ := runReviewArgs(t, rigDir,
		func(req editorial.ReviewRequest) editorial.ReviewResult {
			captured = &req
			return approveGate(t)(req)
		},
		"mq", "review", "--landed", tip, "gt-wisp-somehandle")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if captured == nil {
		t.Fatal("the gate never ran")
	}
	if captured.MRID != "gt-wisp-somehandle" {
		t.Errorf("MRID = %q, want the positional id passed through untouched", captured.MRID)
	}
	if captured.Landed == nil {
		t.Fatal("request.Landed is nil")
	}
}

func TestMQReviewLanded_UsageExitsTwo(t *testing.T) {
	_, _, rigDir := testRigRoot(t, "main")
	code, captured := runReviewArgs(t, rigDir, noGate(t), "mq", "review") // no args, no --landed
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
	if captured != nil {
		t.Error("the gate ran on a usage error")
	}
}

// TestMQReviewLanded_NoFlagStateLeaksBetweenInvocations runs a landed review
// and then a bare invocation in the same process. The second one is a usage
// error only if the first's --landed did not survive it, so this fails if the
// flags are left set — the state leak that otherwise makes the usage test pass
// for the wrong reason once an earlier test has run.
func TestMQReviewLanded_NoFlagStateLeaksBetweenInvocations(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	tip := revForCmd(t, repoDir, "HEAD")

	code, _ := runReviewArgs(t, rigDir, approveGate(t), "mq", "review", "--landed", tip)
	if code != 0 {
		t.Fatalf("landed review exit = %d, want 0", code)
	}

	code, captured := runReviewArgs(t, rigDir, noGate(t), "mq", "review")
	if code != 2 {
		t.Fatalf("exit = %d, want 2: --landed leaked from the previous invocation", code)
	}
	if captured != nil {
		t.Error("the gate ran: the previous invocation's --landed was still set")
	}
}
