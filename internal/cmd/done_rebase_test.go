package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

// fakeRebaseGit lets us drive autoRebaseOnTarget without a real git repo for
// the gating-decision tests.
type fakeRebaseGit struct {
	rebaseErr   error
	rebaseCalls int
	abortCalls  int
}

func (f *fakeRebaseGit) Rebase(onto string) error {
	f.rebaseCalls++
	return f.rebaseErr
}

func (f *fakeRebaseGit) AbortRebase() error {
	f.abortCalls++
	return nil
}

// TestAutoRebaseOnTarget_GatingDecisions verifies the skip/rebase decision
// matrix (gh#3400). The behavior under test is the *decision*, not the actual
// git mechanics — those are exercised separately below against a real repo.
func TestAutoRebaseOnTarget_GatingDecisions(t *testing.T) {
	tests := []struct {
		name          string
		behind        int
		preVerified   bool
		alreadyPushed bool
		wantRebased   bool
		wantSkip      string
		wantCalls     int
	}{
		{
			name:        "not behind: no-op",
			behind:      0,
			wantRebased: false,
			wantSkip:    "",
			wantCalls:   0,
		},
		{
			name:        "behind by 1: rebase runs",
			behind:      1,
			wantRebased: true,
			wantSkip:    "",
			wantCalls:   1,
		},
		{
			name:        "behind by 5: rebase runs",
			behind:      5,
			wantRebased: true,
			wantSkip:    "",
			wantCalls:   1,
		},
		{
			name:        "pre-verified: skip even when behind",
			behind:      3,
			preVerified: true,
			wantRebased: false,
			wantSkip:    "--pre-verified is set",
			wantCalls:   0,
		},
		{
			name:          "already pushed: skip to avoid divergence",
			behind:        3,
			alreadyPushed: true,
			wantRebased:   false,
			wantSkip:      "prior push checkpoint exists",
			wantCalls:     0,
		},
		{
			name:          "pre-verified takes precedence over already-pushed",
			behind:        3,
			preVerified:   true,
			alreadyPushed: true,
			wantRebased:   false,
			wantSkip:      "--pre-verified is set",
			wantCalls:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRebaseGit{}
			rebased, skipReason, err := autoRebaseOnTarget(fake, "origin/main", tt.behind, tt.preVerified, tt.alreadyPushed)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rebased != tt.wantRebased {
				t.Errorf("rebased = %v, want %v", rebased, tt.wantRebased)
			}
			if skipReason != tt.wantSkip {
				t.Errorf("skipReason = %q, want %q", skipReason, tt.wantSkip)
			}
			if fake.rebaseCalls != tt.wantCalls {
				t.Errorf("rebase calls = %d, want %d", fake.rebaseCalls, tt.wantCalls)
			}
			if fake.abortCalls != 0 {
				t.Errorf("abort calls = %d on success path, want 0", fake.abortCalls)
			}
		})
	}
}

// TestAutoRebaseOnTarget_ConflictAborts verifies that a rebase failure causes
// AbortRebase to fire and the returned error includes remediation guidance.
func TestAutoRebaseOnTarget_ConflictAborts(t *testing.T) {
	fake := &fakeRebaseGit{rebaseErr: errors.New("CONFLICT (content): merge conflict in foo.txt")}

	rebased, skipReason, err := autoRebaseOnTarget(fake, "origin/main", 1, false, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rebased {
		t.Error("rebased should be false on conflict")
	}
	if skipReason != "" {
		t.Errorf("skipReason should be empty on conflict, got %q", skipReason)
	}
	if fake.rebaseCalls != 1 {
		t.Errorf("expected 1 rebase call, got %d", fake.rebaseCalls)
	}
	if fake.abortCalls != 1 {
		t.Errorf("expected AbortRebase to fire on conflict, got %d calls", fake.abortCalls)
	}
	// Remediation guidance is part of the contract — agents read this message.
	msg := err.Error()
	if !strings.Contains(msg, "auto-rebase onto origin/main failed") {
		t.Errorf("error missing context: %q", msg)
	}
	if !strings.Contains(msg, "git rebase origin/main") {
		t.Errorf("error missing remediation hint: %q", msg)
	}
	if !strings.Contains(msg, "rerun gt done") {
		t.Errorf("error missing rerun hint: %q", msg)
	}
}

// TestAutoRebaseOnTarget_RealRepoSuccess exercises the rebase against a real
// git working tree to confirm the wiring (Rebase call) actually replays the
// branch onto a moved base. (gh#3400, scenario (a) from the bead notes.)
func TestAutoRebaseOnTarget_RealRepoSuccess(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")

	// Initial commit on main.
	writeRepoFile(t, repo, "README.md", "# initial\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")

	// Branch off and add a polecat commit on a non-conflicting file.
	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "feature.txt", "feature work\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature")

	// Move main forward independently — also non-conflicting with feature.
	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "main-new.txt", "new on main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "advance main")

	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	rebased, skipReason, err := autoRebaseOnTarget(g, "main", 1, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rebased {
		t.Fatalf("expected rebased=true, got false (skip=%q)", skipReason)
	}

	// After rebase, both files must be present and the feature commit must sit
	// on top of the advance-main commit.
	if _, statErr := os.Stat(filepath.Join(repo, "main-new.txt")); statErr != nil {
		t.Errorf("main-new.txt missing after rebase: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "feature.txt")); statErr != nil {
		t.Errorf("feature.txt missing after rebase: %v", statErr)
	}
}

// TestAutoRebaseOnTarget_RealRepoConflictAborts exercises the conflict path
// against a real git working tree: feature and main both touch the same file,
// rebase fails with a CONFLICT, and AbortRebase must restore the working tree
// so the polecat can address the conflict manually. (gh#3400, scenario (b).)
func TestAutoRebaseOnTarget_RealRepoConflictAborts(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")

	writeRepoFile(t, repo, "shared.txt", "v0\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")

	// Feature changes shared.txt to v1.
	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "shared.txt", "v1-feature\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature edit")

	// Main changes shared.txt to a different value — guaranteed conflict.
	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "shared.txt", "v1-main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "main edit")

	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	rebased, skipReason, err := autoRebaseOnTarget(g, "main", 1, false, false)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if rebased {
		t.Error("rebased should be false on conflict")
	}
	if skipReason != "" {
		t.Errorf("skipReason should be empty on conflict, got %q", skipReason)
	}

	// AbortRebase is required to leave the working tree in a clean state. After
	// abort, the rebase-merge dir must be gone — otherwise the polecat is stuck
	// in a half-rebased state.
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "rebase-merge")); !os.IsNotExist(statErr) {
		t.Errorf(".git/rebase-merge should not exist after abort (stat err: %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "rebase-apply")); !os.IsNotExist(statErr) {
		t.Errorf(".git/rebase-apply should not exist after abort (stat err: %v)", statErr)
	}
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// fakeDivergedPushGit lets us drive recoverDivergedPush's decision logic
// without a real git repo.
type fakeDivergedPushGit struct {
	revs    map[string]string
	revErrs map[string]error

	mergeBases   map[[2]string]string
	mergeBaseErr error

	patchIDs   map[[2]string]string
	patchIDErr error

	leaseErr   error
	leaseCalls int
	leaseArgs  []string

	fetchErr   error
	fetchCalls int
}

func (f *fakeDivergedPushGit) Fetch(remote string) error {
	f.fetchCalls++
	return f.fetchErr
}

func (f *fakeDivergedPushGit) Rev(ref string) (string, error) {
	if err, ok := f.revErrs[ref]; ok {
		return "", err
	}
	if sha, ok := f.revs[ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("fakeDivergedPushGit: unknown ref %q", ref)
}

func (f *fakeDivergedPushGit) MergeBase(a, b string) (string, error) {
	if f.mergeBaseErr != nil {
		return "", f.mergeBaseErr
	}
	if base, ok := f.mergeBases[[2]string{a, b}]; ok {
		return base, nil
	}
	return "base", nil
}

func (f *fakeDivergedPushGit) PatchID(base, head string) (string, error) {
	if f.patchIDErr != nil {
		return "", f.patchIDErr
	}
	return f.patchIDs[[2]string{base, head}], nil
}

func (f *fakeDivergedPushGit) PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error {
	f.leaseCalls++
	f.leaseArgs = []string{remote, refspec, branchRef, expectedSHA}
	return f.leaseErr
}

func TestRecoverDivergedPush_FetchFailsAborts(t *testing.T) {
	f := &fakeDivergedPushGit{fetchErr: errors.New("network unreachable")}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if recovered {
		t.Error("must not recover when the pre-comparison fetch fails")
	}
	if diagnosis != "" {
		t.Errorf("diagnosis = %q, want empty (comparison never ran)", diagnosis)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

func TestRecoverDivergedPush_OriginMissingBranch(t *testing.T) {
	f := &fakeDivergedPushGit{
		revErrs: map[string]error{"origin/feature": errors.New("unknown revision")},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if recovered {
		t.Error("must not recover when origin has no ref for the branch")
	}
	if diagnosis != "" {
		t.Errorf("diagnosis = %q, want empty (comparison never ran)", diagnosis)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

func TestRecoverDivergedPush_PatchIdenticalRecovers(t *testing.T) {
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		mergeBases: map[[2]string]string{
			{"origin/main", "origSHA"}:  "base1",
			{"origin/main", "localSHA"}: "base2",
		},
		patchIDs: map[[2]string]string{
			{"base1", "origSHA"}:  "samepatch",
			{"base2", "localSHA"}: "samepatch",
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "diverged by rebase, content identical") {
		t.Errorf("diagnosis = %q, want mention of rebase/content-identical", diagnosis)
	}
	if f.leaseCalls != 1 {
		t.Fatalf("expected 1 leased push, got %d", f.leaseCalls)
	}
	wantArgs := []string{"origin", "feature:feature", "feature", "origSHA"}
	if strings.Join(f.leaseArgs, "|") != strings.Join(wantArgs, "|") {
		t.Errorf("lease args = %v, want %v", f.leaseArgs, wantArgs)
	}
}

func TestRecoverDivergedPush_RealDivergenceRefuses(t *testing.T) {
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDs: map[[2]string]string{
			{"base", "origSHA"}:  "patchA",
			{"base", "localSHA"}: "patchB",
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: patch-ids differ, this is real divergence")
	}
	if !strings.Contains(diagnosis, "NOT patch-identical") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run on real divergence, got %d calls", f.leaseCalls)
	}
}

func TestRecoverDivergedPush_LeaseFails(t *testing.T) {
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDs: map[[2]string]string{
			{"base", "origSHA"}:  "samepatch",
			{"base", "localSHA"}: "samepatch",
		},
		leaseErr: errors.New("stale info"),
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if recovered {
		t.Fatal("recovered must be false when the leased push itself fails")
	}
	if !strings.Contains(diagnosis, "diverged by rebase, content identical") {
		t.Errorf("diagnosis = %q, want it set even though the lease push failed", diagnosis)
	}
	if err == nil || !strings.Contains(err.Error(), "leased force-push failed") {
		t.Errorf("err = %v, want it to wrap the lease failure", err)
	}
	if f.leaseCalls != 1 {
		t.Errorf("expected 1 lease attempt, got %d", f.leaseCalls)
	}
}

// TestRecoverDivergedPush_RealRepo exercises the full scenario end to end
// against real git repos: a branch pushed by one dispatch, main advancing,
// then a second dispatch reusing the branch and rebasing it onto origin/main
// (the formula's branch-reuse step) before gt done's plain push fails
// non-fast-forward. Recovery must land the rebased tip on origin without
// losing either commit's content. (gt-bf5x)
func TestRecoverDivergedPush_RealRepo(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

	seed := filepath.Join(tmp, "seed")
	testRunGit(t, tmp, "init", "--initial-branch", "main", seed)
	testRunGit(t, seed, "config", "user.email", "test@test.com")
	testRunGit(t, seed, "config", "user.name", "Test")
	writeRepoFile(t, seed, "README.md", "# initial\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "initial")
	testRunGit(t, seed, "remote", "add", "origin", remote)
	testRunGit(t, seed, "push", "origin", "main")

	// First dispatch: branch, do work, push (this is what lands on origin
	// before the branch gets reused by a later dispatch).
	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "feature.txt", "feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature work")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	// main advances independently while the MR sits in the queue.
	testRunGit(t, seed, "checkout", "main")
	writeRepoFile(t, seed, "main-new.txt", "advance\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "advance main")
	testRunGit(t, seed, "push", "origin", "main")

	// Second dispatch: fresh checkout of the reused branch, rebased onto
	// origin/main per the formula's branch-reuse step — diverges history from
	// origin while keeping the content identical.
	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "-b", "feature", "origin/feature")
	testRunGit(t, work, "fetch", "origin")
	testRunGit(t, work, "rebase", "origin/main")

	g := gitpkg.NewGit(work)

	// The primary non-force push (what gt done tries first) must fail
	// non-fast-forward, exactly as observed in the bead.
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward after rebase")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "diverged by rebase, content identical") {
		t.Errorf("diagnosis = %q, want mention of rebase/content-identical", diagnosis)
	}

	verify := filepath.Join(tmp, "verify")
	testRunGit(t, tmp, "clone", "--branch", "feature", remote, verify)
	if _, statErr := os.Stat(filepath.Join(verify, "main-new.txt")); statErr != nil {
		t.Errorf("main-new.txt missing on origin/feature after recovery: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(verify, "feature.txt")); statErr != nil {
		t.Errorf("feature.txt missing on origin/feature after recovery: %v", statErr)
	}
}

// TestRecoverDivergedPush_RealRepoRefusesGenuineDivergence guards the safety
// side: when origin's tip is real, different work (not the same content
// rebased), recovery must refuse and leave origin untouched rather than
// clobber it.
func TestRecoverDivergedPush_RealRepoRefusesGenuineDivergence(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

	seed := filepath.Join(tmp, "seed")
	testRunGit(t, tmp, "init", "--initial-branch", "main", seed)
	testRunGit(t, seed, "config", "user.email", "test@test.com")
	testRunGit(t, seed, "config", "user.name", "Test")
	writeRepoFile(t, seed, "README.md", "# initial\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "initial")
	testRunGit(t, seed, "remote", "add", "origin", remote)
	testRunGit(t, seed, "push", "origin", "main")

	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "feature.txt", "feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature work")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "feature")
	writeRepoFile(t, work, "feature-more.txt", "more local work\n")
	testRunGit(t, work, "add", ".")
	testRunGit(t, work, "commit", "-m", "more feature work")

	// Someone else pushes genuinely different content to the same branch
	// concurrently.
	other := filepath.Join(tmp, "other")
	testRunGit(t, tmp, "clone", remote, other)
	testRunGit(t, other, "config", "user.email", "test@test.com")
	testRunGit(t, other, "config", "user.name", "Test")
	testRunGit(t, other, "checkout", "feature")
	writeRepoFile(t, other, "someone-elses-work.txt", "different content\n")
	testRunGit(t, other, "add", ".")
	testRunGit(t, other, "commit", "-m", "someone else's real work")
	testRunGit(t, other, "push", "origin", "feature:feature")
	otherHead := gitpkgRev(t, other, "HEAD")

	g := gitpkg.NewGit(work)
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: origin has genuinely different content, not just a rebase")
	}
	if !strings.Contains(diagnosis, "NOT patch-identical") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}

	testRunGit(t, work, "fetch", "origin")
	if got := gitpkgRev(t, work, "origin/feature"); got != otherHead {
		t.Errorf("origin/feature was modified: got %s, want %s (untouched)", got, otherHead)
	}
}

func gitpkgRev(t *testing.T, dir, ref string) string {
	t.Helper()
	sha, err := gitpkg.NewGit(dir).Rev(ref)
	if err != nil {
		t.Fatalf("rev %s in %s: %v", ref, dir, err)
	}
	return sha
}
