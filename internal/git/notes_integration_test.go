//go:build integration

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// commitFile writes content to path in dir, stages it, and commits with message.
func commitFile(t *testing.T, dir, path, content, message string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	g := NewGit(dir)
	if err := g.Add(path); err != nil {
		t.Fatalf("add %s: %v", path, err)
	}
	if err := g.Commit(message); err != nil {
		t.Fatalf("commit %s: %v", message, err)
	}
	rev, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return rev
}

func TestIntegrationPatchID_StableAcrossNoOpRebase(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	before, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID before: %v", err)
	}
	if before == "" {
		t.Fatal("PatchID returned empty string")
	}

	// Simulate a real rebase: upstream moves forward with an unrelated commit,
	// sibling to head off the same original base (giving the new base a
	// different sha from the old one), then the feature commit is replayed
	// on top of it. The diff content is unchanged, so the patch-id must
	// match even though both the base and head shas differ from before.
	cmd := exec.Command("git", "checkout", "-b", "upstream", base)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout upstream: %v\n%s", err, out)
	}
	newBase := commitFile(t, dir, "unrelated.txt", "upstream progress\n", "unrelated upstream commit")

	cmd = exec.Command("git", "checkout", "-b", "rebased", newBase)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout rebased: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "cherry-pick", head)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cherry-pick: %v\n%s", err, out)
	}
	newHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD after cherry-pick: %v", err)
	}
	if newHead == head {
		t.Fatal("expected cherry-pick to produce a new commit sha")
	}

	after, err := g.PatchID(newBase, newHead)
	if err != nil {
		t.Fatalf("PatchID after: %v", err)
	}
	if after != before {
		t.Fatalf("PatchID changed across no-op rebase: before=%s after=%s", before, after)
	}
}

// TestPatchIDVerbatim_DistinguishesWhitespaceOnlyEdit is gt-2colr from both
// sides in one run: PatchID cannot tell a rework that answers a whitespace
// finding from the attempt that was rejected for it, and PatchIDVerbatim can.
// The first assertion is a premise, not a wish — if a future git stops
// stripping whitespace, it fails and names the workaround that is now dead.
//
// The added line is the whole diff, so its trailing space is the diff's last
// byte: this is also the case that fails if the diff ever reaches patch-id
// through run()'s trim again, which both modes read as a shorter line.
func TestIntegrationPatchIDVerbatim_DistinguishesWhitespaceOnlyEdit(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	// Two tips off one base whose diffs differ only in the trailing space of
	// the added line: the rejected attempt and the fix that strips it.
	rejected := commitFile(t, dir, "feature.txt", "work \n", "implement the feature")
	cmd := exec.Command("git", "checkout", base)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout %s: %v\n%s", base, err, out)
	}
	fixed := commitFile(t, dir, "feature.txt", "work\n", "fix: strip the trailing whitespace")

	stableRejected, err := g.PatchID(base, rejected)
	if err != nil {
		t.Fatalf("PatchID rejected tip: %v", err)
	}
	stableFixed, err := g.PatchID(base, fixed)
	if err != nil {
		t.Fatalf("PatchID fixed tip: %v", err)
	}
	if stableRejected != stableFixed {
		t.Fatalf("PatchID now distinguishes a whitespace-only edit (%s vs %s); the gt-2colr workaround in PatchIDVerbatim may be removable",
			stableRejected, stableFixed)
	}

	verbatimRejected, err := g.PatchIDVerbatim(base, rejected)
	if err != nil {
		t.Fatalf("PatchIDVerbatim rejected tip: %v", err)
	}
	verbatimFixed, err := g.PatchIDVerbatim(base, fixed)
	if err != nil {
		t.Fatalf("PatchIDVerbatim fixed tip: %v", err)
	}
	if verbatimRejected == verbatimFixed {
		t.Fatalf("PatchIDVerbatim reads a whitespace-only fix as no change: both %s", verbatimRejected)
	}
}

// currentBranchName reports the branch initTestRepo left checked out, so
// these tests do not depend on git's configured default branch name.
func currentBranchName(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse --abbrev-ref HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPatchIDs_PerCommitAndStableAcrossRebase pins what PatchIDs adds over
// PatchID: one id per commit, each unchanged by a rebase, so a caller can tell
// which individual changes two branches share.
func TestIntegrationPatchIDs_PerCommitAndStableAcrossRebase(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	a := commitFile(t, dir, "a.txt", "a\n", "add a")
	b := commitFile(t, dir, "b.txt", "b\n", "add b")

	before, err := g.PatchIDs(base, b)
	if err != nil {
		t.Fatalf("PatchIDs before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("PatchIDs returned %d ids for a 2-commit range: %v", len(before), before)
	}

	// Replay the same two commits onto a moved base: every sha changes, every
	// patch-id must not.
	runGit(t, dir, "checkout", "-b", "upstream", base)
	newBase := commitFile(t, dir, "upstream.txt", "upstream progress\n", "unrelated upstream commit")
	runGit(t, dir, "checkout", "-b", "rebased", newBase)
	runGit(t, dir, "cherry-pick", a, b)
	newHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD after cherry-pick: %v", err)
	}

	after, err := g.PatchIDs(newBase, newHead)
	if err != nil {
		t.Fatalf("PatchIDs after: %v", err)
	}
	if sorted := sortedCopy(after); !equalStrings(sorted, sortedCopy(before)) {
		t.Fatalf("per-commit patch-ids changed across a rebase:\nbefore=%v\nafter=%v", before, after)
	}
}

// TestFirstParentPatchIDs_MergeCommitCarriesBranchPatchID pins the property
// the refinery's landed-commit lookup depends on (gt-9t0p): a landing merge
// commit's own patch-id, measured against its first parent, is the whole
// merged branch's cumulative patch-id — the value a reviewed range's note is
// keyed to. Without that, the commit that landed an MR whose sha a rebase
// rewrote could not be found at all.
func TestIntegrationFirstParentPatchIDs_MergeCommitCarriesBranchPatchID(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	mainBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	runGit(t, dir, "checkout", "-b", "feature", base)
	featureHead := commitFile(t, dir, "feature.txt", "feature\n", "feat: add feature")
	runGit(t, dir, "checkout", mainBranch)
	other := commitFile(t, dir, "other.txt", "other\n", "other: unrelated landing")
	runGit(t, dir, "merge", "--no-ff", "feature", "-m", "Merge feature into main")
	merge, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev merge: %v", err)
	}

	branchPatchID, err := g.PatchID(base, featureHead)
	if err != nil {
		t.Fatalf("PatchID branch range: %v", err)
	}

	pairs, err := g.FirstParentPatchIDs(base, "HEAD")
	if err != nil {
		t.Fatalf("FirstParentPatchIDs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("FirstParentPatchIDs over a moved-base merge = %d entries (%v), want 2 (the merge and the landing before it)", len(pairs), pairs)
	}
	if pairs[0].Commit != merge {
		t.Fatalf("newest entry is %s, want the merge commit %s", pairs[0].Commit, merge)
	}
	if pairs[0].PatchID != branchPatchID {
		t.Fatalf("merge commit patch-id = %s, want the merged branch's cumulative patch-id %s", pairs[0].PatchID, branchPatchID)
	}
	if pairs[1].Commit != other {
		t.Fatalf("oldest entry is %s, want the landing before it %s", pairs[1].Commit, other)
	}
	for _, p := range pairs {
		if p.Commit == featureHead {
			t.Fatal("the branch commit is not on the first-parent chain and must not be returned")
		}
	}

	// The same patch replayed as a cherry-pick has a different sha and the
	// same patch-id, which is what makes the lookup survive a rewritten sha.
	runGit(t, dir, "checkout", "-b", "moved", base)
	movedBase := commitFile(t, dir, "upstream.txt", "upstream progress\n", "unrelated upstream commit")
	runGit(t, dir, "checkout", "-b", "picked", movedBase)
	runGit(t, dir, "cherry-pick", featureHead)
	picked, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev picked: %v", err)
	}
	if picked == featureHead {
		t.Fatal("cherry-pick should have rewritten the commit sha")
	}
	pickedPairs, err := g.FirstParentPatchIDs(movedBase, picked)
	if err != nil {
		t.Fatalf("FirstParentPatchIDs picked: %v", err)
	}
	if len(pickedPairs) != 1 || pickedPairs[0].Commit != picked || pickedPairs[0].PatchID != branchPatchID {
		t.Fatalf("cherry-picked commit = %v, want one entry {%s %s}", pickedPairs, branchPatchID, picked)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
