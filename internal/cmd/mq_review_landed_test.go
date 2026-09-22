package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// runReviewArgs drives the real gt mq review command tree (rootCmd, cobra's
// own arg parsing, runMQReview's exit-2 paths and os.Exit call) and returns
// its exit code. A fake for the editorial gate is installed for the duration
// of the call so the test never invokes om.
func runReviewArgs(t *testing.T, rigDir string, gate func(req editorial.ReviewRequest) editorial.ReviewResult, args ...string) (int, *editorial.ReviewRequest) {
	t.Helper()
	t.Chdir(rigDir)

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

	oldExecute := Execute
	Execute = func() int { return CodeOf(t, rootCmd) }
	defer func() { Execute = oldExecute }()

	return Execute(), captured
}

// CodeOf runs a cobra command and maps its error onto the CLI's exit-code
// contract the same way Execute does: a SilentExitError carries its code,
// any other error is exit 1.
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

// testRigRoot lays out a minimal Gas Town town in a temp dir: a workspace
// marker, one rig with a beads dir, a polecat subdir to stand in for the
// caller's cwd, and the rig's refinery clone with a remote and a landed
// commit. It returns the caller cwd (which the caller chdirs into) and the
// repo and rig dirs.
func testRigRoot(t *testing.T, defaultBranch string) (cwd, repoDir, rigDir string) {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rigDir = filepath.Join(town, "gastown")
	repoDir = filepath.Join(rigDir, "refinery", "rig")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	cwd = filepath.Join(rigDir, "polecats", "shale")
	if err := os.MkdirAll(cwd, 0755); err != nil {
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
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(repoDir, "add", ".")
	run(repoDir, "commit", "-m", "landed work")
	run(repoDir, "push", "origin", defaultBranch)

	t.Chdir(cwd)
	return cwd, repoDir, rigDir
}

func revForCmd(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", ref).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return string(out) // trimmed: Output strips the newline
}

func TestMQReviewLanded_RefusalExitsTwo(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	// The base commit is not reachable from origin/main (the tip is its
	// child), so --landed refuses it.
	base := revForCmd(t, repoDir, "HEAD^")

	code, captured := runReviewArgs(t, rigDir,
		func(req editorial.ReviewRequest) editorial.ReviewResult {
			t.Error("the gate must not run when the range is refused")
			return editorial.ReviewResult{Exit: 0}
		},
		"mq", "review", "--landed", base)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (a refused range is a config error, never request_changes)", code)
	}
	if captured != nil {
		t.Error("the gate ran despite a refused range")
	}
}

func TestMQReviewLanded_ApproveExitsZeroAndResolvesRange(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	tip := revForCmd(t, repoDir, "HEAD")

	var captured *editorial.ReviewRequest
	code, _ := runReviewArgs(t, rigDir,
		func(req editorial.ReviewRequest) editorial.ReviewResult {
			captured = &req
			return editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Verdict: "approve"}}
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
	if captured.RehearsedHead != "" {
		t.Errorf("RehearsedHead = %q, want empty (a landed review rehearses nothing)", captured.RehearsedHead)
	}
	if captured.Rig != "gastown" || captured.Target != "main" {
		t.Errorf("Rig/Target = %q/%q, want gastown/main", captured.Rig, captured.Target)
	}
	if captured.MRID == "" {
		t.Error("an unset positional MR id must be filled by a minted wisp")
	}
}

func TestMQReviewLanded_RequestChangesExitsOne(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	tip := revForCmd(t, repoDir, "HEAD")

	code, _ := runReviewArgs(t, rigDir,
		func(req editorial.ReviewRequest) editorial.ReviewResult {
			return editorial.ReviewResult{Exit: 1, Note: &editorial.Note{Verdict: "request_changes"}}
		},
		"mq", "review", "--landed", tip)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (request_changes)", code)
	}
}

func TestMQReviewLanded_PositionalMRID(t *testing.T) {
	_, repoDir, rigDir := testRigRoot(t, "main")
	tip := revForCmd(t, repoDir, "HEAD")

	bd := beads.New(rigDir)
	handle, err := bd.Create(beads.CreateOptions{
		Title:     "handle",
		Ephemeral: true,
		Rig:       "gastown",
	})
	if err != nil {
		t.Fatalf("create handle wisp: %v", err)
	}

	var captured *editorial.ReviewRequest
	code, _ := runReviewArgs(t, rigDir,
		func(req editorial.ReviewRequest) editorial.ReviewResult {
			captured = &req
			return editorial.ReviewResult{Exit: 0}
		},
		"mq", "review", "--landed", tip, handle.ID)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if captured == nil {
		t.Fatal("the gate never ran")
	}
	if captured.MRID != handle.ID {
		t.Errorf("MRID = %q, want the positional %q", captured.MRID, handle.ID)
	}
	if captured.Landed == nil {
		t.Fatal("request.Landed is nil")
	}
}

func TestMQReviewLanded_UsageExitsTwo(t *testing.T) {
	_, _, rigDir := testRigRoot(t, "main")
	code, _ := runReviewArgs(t, rigDir,
		func(req editorial.ReviewRequest) editorial.ReviewResult {
			t.Error("the gate must not run on a usage error")
			return editorial.ReviewResult{}
		},
		"mq", "review") // no args, no --landed
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
}

// keep git imported: revForCmd is git-binary-based, but the cmd package's
// helpers under test touch git.Git via doMQReviewLanded.
var _ = git.NewGit
var _ = cobra.RangeArgs
var _ = beads.ErrNotFound
