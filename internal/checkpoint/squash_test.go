package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a fresh git repo with an initial commit and returns its path.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args[1:], err, out)
		}
	}

	// Create initial commit on main
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "initial commit"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args[1:], err, out)
		}
	}

	return dir
}

// createBranch creates a branch from current HEAD and switches to it.
func createBranch(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", "-b", branch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout -b %s failed: %v\n%s", branch, err, out)
	}
}

// addCommit adds a file and commits with the given message.
func addCommit(t *testing.T, dir, filename, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", filename},
		{"git", "commit", "-m", msg},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args[1:], err, out)
		}
	}
}

// writeRepoFile writes a file into dir.
func writeRepoFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// mustGit runs a git command in dir, failing the test if it exits non-zero.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// getCommitSubjects returns the commit subjects on the branch since main.
func getCommitSubjects(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "log", "--format=%s", "main..HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func TestCountWIPCommits_NoWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	addCommit(t, dir, "b.go", "package b", "add feature B")

	count, err := CountWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 WIP commits, got %d", count)
	}
}

func TestCountWIPCommits_AllWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)

	count, err := CountWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 WIP commits, got %d", count)
	}
}

func TestCountWIPCommits_Mixed(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "real work")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)
	addCommit(t, dir, "c.go", "package c", "more real work")

	count, err := CountWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 WIP commit, got %d", count)
	}
}

func TestSquashWIPCommits_NoWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "real work")

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 0 {
		t.Errorf("expected 0, got %d", wipCount)
	}

	// Verify commit is untouched
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 || subjects[0] != "real work" {
		t.Errorf("expected [real work], got %v", subjects)
	}
}

func TestSquashWIPCommits_AllWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 2 {
		t.Errorf("expected 2, got %d", wipCount)
	}

	// Verify squashed into single commit with generic message
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 {
		t.Errorf("expected 1 commit after squash, got %d: %v", len(subjects), subjects)
	}
	if len(subjects) > 0 && subjects[0] != "squashed WIP checkpoint commits" {
		t.Errorf("expected generic message, got %q", subjects[0])
	}

	// Verify files exist
	for _, f := range []string{"a.go", "b.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist after squash", f)
		}
	}
}

func TestSquashWIPCommits_Mixed(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "implement auth handler")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)
	addCommit(t, dir, "c.go", "package c", "add auth tests")
	addCommit(t, dir, "d.go", "package d", WIPCommitPrefix)

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 2 {
		t.Errorf("expected 2, got %d", wipCount)
	}

	// Verify squashed into single commit with non-WIP subjects preserved
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 {
		t.Errorf("expected 1 commit after squash, got %d: %v", len(subjects), subjects)
	}

	// Verify all files exist
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist after squash", f)
		}
	}
}

func TestIsAutoSaveSubject(t *testing.T) {
	cases := []struct {
		subject string
		want    bool
	}{
		{WIPCommitPrefix, true},
		{"WIP: checkpoint (auto) 2026-09-08", true},
		{"fix: auto-save uncommitted implementation work (gt-pvx safety net)", true},
		{"fix: auto-save uncommitted implementation work (gt-wov, gt-pvx safety net)", true},
		{"fix: real bug in auto-save handling", false},
		{"implement feature", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsAutoSaveSubject(c.subject); got != c.want {
			t.Errorf("IsAutoSaveSubject(%q) = %v, want %v", c.subject, got, c.want)
		}
	}
}

func TestSquashAutoSaveCommits_AllGenerated_UsesFallbackTitle(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", "fix: auto-save uncommitted implementation work (gt-abc, gt-pvx safety net)")

	count, err := SquashAutoSaveCommits(dir, "main", "fix: handle nil pointer in auth (gt-abc)")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 squashed, got %d", count)
	}

	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 {
		t.Fatalf("expected 1 commit after squash, got %d: %v", len(subjects), subjects)
	}
	if subjects[0] != "fix: handle nil pointer in auth (gt-abc)" {
		t.Errorf("expected fallback title as subject, got %q", subjects[0])
	}

	for _, f := range []string{"a.go", "b.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist after squash", f)
		}
	}
}

func TestSquashAutoSaveCommits_AllGenerated_EmptyFallback(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "fix: auto-save uncommitted implementation work (gt-pvx safety net)")

	count, err := SquashAutoSaveCommits(dir, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 squashed, got %d", count)
	}
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 || subjects[0] != "squashed auto-save checkpoint commits" {
		t.Errorf("expected generic subject, got %v", subjects)
	}
}

func TestSquashAutoSaveCommits_Mixed_KeepsRealSubject(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "implement auth handler (gt-abc)")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)
	addCommit(t, dir, "c.go", "package c", "fix: auto-save uncommitted implementation work (gt-pvx safety net)")

	count, err := SquashAutoSaveCommits(dir, "main", "fallback title")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 squashed, got %d", count)
	}

	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 {
		t.Fatalf("expected 1 commit after squash, got %d: %v", len(subjects), subjects)
	}
	if subjects[0] != "implement auth handler (gt-abc)" {
		t.Errorf("expected real subject preserved as title, got %q", subjects[0])
	}

	for _, f := range []string{"a.go", "b.go", "c.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist after squash", f)
		}
	}
}

func TestSquashAutoSaveCommits_NoGenerated_Untouched(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	addCommit(t, dir, "b.go", "package b", "add feature B")

	count, err := SquashAutoSaveCommits(dir, "main", "fallback title")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 squashed, got %d", count)
	}
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 2 {
		t.Errorf("expected history untouched (2 commits), got %v", subjects)
	}
}

func TestSquashWIPCommits_NoCommits(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 0 {
		t.Errorf("expected 0 for no commits, got %d", wipCount)
	}
}

// TestHasAutoSaveCommits_BaseRefMissing covers gt-c1mw: callers that treat a
// zero-value (false, nil-checked-away) return as "no auto-save commits" must
// see an actual error here, not a silent false. An unresolvable base ref is
// the simplest way to make merge-base fail.
func TestHasAutoSaveCommits_BaseRefMissing(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)

	has, err := HasAutoSaveCommits(dir, "no-such-base-ref", "feature")
	if err == nil {
		t.Fatal("expected an error when the base ref cannot be resolved, got nil")
	}
	if has {
		t.Error("expected false alongside the error")
	}
}

func TestHasAutoSaveCommits_None(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")

	has, err := HasAutoSaveCommits(dir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no auto-save commits")
	}
}

func TestHasAutoSaveCommits_WIPTip(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)

	has, err := HasAutoSaveCommits(dir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected an auto-save commit to be detected")
	}
}

func TestHasAutoSaveCommits_WIPNotTip(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", "fix: finish the feature")

	has, err := HasAutoSaveCommits(dir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected an auto-save commit buried earlier in the range to be detected")
	}
}

// HasAutoSaveCommits does not check anything out; it must leave the working
// tree exactly where it found it, unlike the squash helpers above.
func TestHasAutoSaveCommits_DoesNotMutateRepo(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	before := getCommitSubjects(t, dir)

	if _, err := HasAutoSaveCommits(dir, "main", "feature"); err != nil {
		t.Fatal(err)
	}

	after := getCommitSubjects(t, dir)
	if len(before) != len(after) || before[0] != after[0] {
		t.Errorf("HasAutoSaveCommits mutated history: before=%v after=%v", before, after)
	}
}

func TestInspectAutoSaveTip_RealTip(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	addCommit(t, dir, "b.go", "package b", "fix: finish feature A")

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip.AutoSave {
		t.Errorf("expected a real tip, got AutoSave with subject %q", tip.Subject)
	}
	if tip.Trailing != 0 {
		t.Errorf("expected 0 trailing auto-save commits, got %d", tip.Trailing)
	}
	if tip.Ahead != 2 {
		t.Errorf("expected 2 commits ahead, got %d", tip.Ahead)
	}
	if tip.Subject != "fix: finish feature A" {
		t.Errorf("expected the tip subject, got %q", tip.Subject)
	}
}

// The gt-iki6 shape: real work with a checkpoint_dog commit on top of it.
func TestInspectAutoSaveTip_WIPTip(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !tip.AutoSave {
		t.Fatalf("expected the tip to be detected as machine-generated, got %q", tip.Subject)
	}
	if tip.Trailing != 1 {
		t.Errorf("expected 1 trailing auto-save commit, got %d", tip.Trailing)
	}
	if tip.Ahead != 2 {
		t.Errorf("expected 2 commits ahead, got %d", tip.Ahead)
	}
}

func TestInspectAutoSaveTip_AllGenerated(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", AutoSaveCommitPrefix)

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !tip.AutoSave {
		t.Fatal("expected the tip to be detected as machine-generated")
	}
	if tip.Trailing != tip.Ahead || tip.Ahead != 2 {
		t.Errorf("expected the whole branch to be machine-generated (2 of 2), got %d of %d", tip.Trailing, tip.Ahead)
	}
}

// A machine-generated commit buried under real work leaves the tip
// submittable, so Trailing counts only the run at the tip.
func TestInspectAutoSaveTip_WIPNotTip(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", "fix: finish the feature")

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip.AutoSave {
		t.Errorf("expected a real tip, got %q", tip.Subject)
	}
	if tip.Trailing != 0 {
		t.Errorf("expected 0 trailing auto-save commits, got %d", tip.Trailing)
	}
	if tip.Ahead != 2 {
		t.Errorf("expected 2 commits ahead, got %d", tip.Ahead)
	}
}

func TestInspectAutoSaveTip_NoCommits(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip != (AutoSaveTip{}) {
		t.Errorf("expected the zero value for a branch with no commits, got %+v", tip)
	}
}

// An empty tip subject must not read as the commit beneath it: naming the
// wrong commit makes the refusal's rewrite advice target the wrong HEAD~N.
func TestInspectAutoSaveTip_EmptyTipSubject(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	writeRepoFile(t, dir, "b.go", "package b")
	mustGit(t, dir, "add", "b.go")
	mustGit(t, dir, "commit", "--allow-empty-message", "-m", "")

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip.Ahead != 2 {
		t.Errorf("expected 2 commits ahead, got %d", tip.Ahead)
	}
	if tip.Subject != "" {
		t.Errorf("expected the empty tip subject, got %q", tip.Subject)
	}
	if tip.AutoSave {
		t.Error("expected an empty subject not to read as machine-generated")
	}
}

// The commit the run folds into decides whether an amend keeps a real message,
// so a merge there must be visible to the caller.
func TestInspectAutoSaveTip_MergeBeneathTheRun(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "side")
	addCommit(t, dir, "side.go", "package side", "add side work")
	mustGit(t, dir, "checkout", "main")
	createBranch(t, dir, "feature")
	mustGit(t, dir, "merge", "--no-ff", "-m", "Merge branch 'side'", "side")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !tip.AutoSave || tip.Trailing != 1 {
		t.Fatalf("expected a single machine-generated tip, got %+v", tip)
	}
	if !tip.BeneathIsMerge {
		t.Errorf("expected the merge beneath the run to be reported, got %+v", tip)
	}
}

func TestInspectAutoSaveTip_RealCommitBeneathTheRun(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip.BeneathIsMerge {
		t.Errorf("expected a non-merge commit beneath the run, got %+v", tip)
	}
}

// InspectAutoSaveTip never checks anything out or rewrites history, so callers
// may run it on a branch they are about to submit.
func TestInspectAutoSaveTip_DoesNotMutateRepo(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)
	before := getCommitSubjects(t, dir)

	if _, err := InspectAutoSaveTip(dir, "main", "HEAD"); err != nil {
		t.Fatal(err)
	}

	after := getCommitSubjects(t, dir)
	if len(before) != len(after) {
		t.Fatalf("InspectAutoSaveTip mutated history: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("InspectAutoSaveTip mutated history: before=%v after=%v", before, after)
		}
	}
}
