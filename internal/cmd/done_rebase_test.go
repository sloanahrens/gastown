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

	// patchIDLists backs FirstParentPatchIDs (one id per commit in the range).
	// The fake pairs each id with a placeholder sha — recoverDivergedPush never
	// reads the commit half.
	patchIDLists map[[2]string][]string
	patchIDsErr  error

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

func (f *fakeDivergedPushGit) FirstParentPatchIDs(base, head string) ([]gitpkg.PatchIDCommit, error) {
	if f.patchIDsErr != nil {
		return nil, f.patchIDsErr
	}
	ids := f.patchIDLists[[2]string{base, head}]
	if ids == nil {
		return nil, nil
	}
	pairs := make([]gitpkg.PatchIDCommit, len(ids))
	for i, id := range ids {
		pairs[i] = gitpkg.PatchIDCommit{PatchID: id, Commit: fmt.Sprintf("commit-%d", i)}
	}
	return pairs, nil
}

func (f *fakeDivergedPushGit) PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error {
	f.leaseCalls++
	f.leaseArgs = []string{remote, refspec, branchRef, expectedSHA}
	return f.leaseErr
}

func TestRecoverDivergedPush_FetchFailsAborts(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		patchIDLists: map[[2]string][]string{
			{"base1", "origSHA"}:  {"p1"},
			{"base2", "localSHA"}: {"p1"},
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
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDs: map[[2]string]string{
			{"base", "origSHA"}:  "patchA",
			{"base", "localSHA"}: "patchB",
		},
		patchIDLists: map[[2]string][]string{
			{"base", "origSHA"}:  {"patchA"},
			{"base", "localSHA"}: {"patchB"},
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: origin holds a change local does not, this is real divergence")
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}
	if !strings.Contains(diagnosis, "patchA") {
		t.Errorf("diagnosis = %q, want the unaccounted-for patch-id named", diagnosis)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run on real divergence, got %d calls", f.leaseCalls)
	}
}

// TestRecoverDivergedPush_ReworkRedispatchRecovers is the gt-i0z3 case: the
// branch was pushed by an earlier dispatch, the new dispatch rebased it *and*
// added a fix commit, so the two ranges are not patch-identical while every
// change origin holds is also held locally. Refusing here (as the pure-rebase
// check did) is a false "possible work loss" alarm on the exact path every
// resume takes.
func TestRecoverDivergedPush_ReworkRedispatchRecovers(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDs: map[[2]string]string{
			{"base", "origSHA"}:  "rangePatchOld",
			{"base", "localSHA"}: "rangePatchNew",
		},
		patchIDLists: map[[2]string][]string{
			// origin: the two commits the first dispatch pushed.
			{"base", "origSHA"}: {"p1", "p2"},
			// local: the same two, rebased, plus the rework fix commit.
			{"base", "localSHA"}: {"p1", "p2", "p3-fix"},
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery on the rework-redispatch path, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "rework-redispatch") || !strings.Contains(diagnosis, "no origin work lost") {
		t.Errorf("diagnosis = %q, want it to name the rework case and rule out work loss", diagnosis)
	}
	if f.leaseCalls != 1 {
		t.Fatalf("expected 1 leased push, got %d", f.leaseCalls)
	}
}

// TestRecoverDivergedPush_SquashedButIdenticalRecovers covers the one case the
// per-commit comparison cannot see: the branch carries byte-identical content
// but a different commit split (squashed, or amended). Every origin patch-id
// then looks unaccounted for, so the whole-range comparison has to be the one
// that clears it.
func TestRecoverDivergedPush_SquashedButIdenticalRecovers(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDs: map[[2]string]string{
			{"base", "origSHA"}:  "rangePatch",
			{"base", "localSHA"}: "rangePatch",
		},
		patchIDLists: map[[2]string][]string{
			{"base", "origSHA"}:  {"p1", "p2"},
			{"base", "localSHA"}: {"p1p2-squashed"},
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery: identical range content, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "content identical") {
		t.Errorf("diagnosis = %q, want mention of identical content", diagnosis)
	}
}

// TestRecoverDivergedPush_OriginCommitNotLocallyHeldRefuses pins the safety
// rule on the rework path: containing *some* of origin's work is not enough,
// because containing all of it is what makes the force-push lossless.
func TestRecoverDivergedPush_OriginCommitNotLocallyHeldRefuses(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDs: map[[2]string]string{
			{"base", "origSHA"}:  "rangePatchOld",
			{"base", "localSHA"}: "rangePatchNew",
		},
		patchIDLists: map[[2]string][]string{
			{"base", "origSHA"}: {"p1", "someone-elses"},
			// The rework fix is present, but origin's p2 is missing locally.
			{"base", "localSHA"}: {"p1", "p3-fix"},
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: origin holds a change local does not")
	}
	if !strings.Contains(diagnosis, "real divergence") || !strings.Contains(diagnosis, "someone-elses") {
		t.Errorf("diagnosis = %q, want real divergence naming the unaccounted-for commit", diagnosis)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run when origin work is missing, got %d calls", f.leaseCalls)
	}
}

// TestRecoverDivergedPush_OriginHasNoCommitsOfItsOwn covers a remote ref that
// carries nothing of its own against the target: there is no origin work to
// lose, so the push is safe to lease even though nothing about it matches.
// PatchID cannot answer this case at all (an empty range has no diff to hash),
// which is why the emptiness is handled before the range comparison.
func TestRecoverDivergedPush_OriginHasNoCommitsOfItsOwn(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDLists: map[[2]string][]string{
			{"base", "localSHA"}: {"p1"},
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery: origin carries no commits of its own, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "no origin work at risk") {
		t.Errorf("diagnosis = %q, want it to say there is no origin work at risk", diagnosis)
	}
	if f.leaseCalls != 1 {
		t.Fatalf("expected 1 leased push, got %d", f.leaseCalls)
	}
}

// TestRecoverDivergedPush_MissingRangePatchIDIsNotIdentity pins the other half
// of that rule: when the whole-range comparison yields no id at all, it has
// proven nothing, so the per-commit comparison must be the one that decides
// (and here it refuses).
func TestRecoverDivergedPush_MissingRangePatchIDIsNotIdentity(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		// No patchIDs entries: PatchID answers with "" for both ranges.
		patchIDLists: map[[2]string][]string{
			{"base", "origSHA"}:  {"p1"},
			{"base", "localSHA"}: {"p2"},
		},
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: an empty patch-id is not proof of identical content")
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}
}

func TestRecoverDivergedPush_PatchIDsErrorAborts(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDsErr: errors.New("git log failed"),
	}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main")
	if recovered {
		t.Error("must not recover when the commit comparison itself failed")
	}
	if diagnosis != "" {
		t.Errorf("diagnosis = %q, want empty (no comparison result to report)", diagnosis)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

func TestPatchIDsMissingFrom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		want        []string
		have        []string
		wantMissing []string
	}{
		{
			name: "all accounted for",
			want: []string{"p1", "p2"},
			have: []string{"p1", "p2", "p3"},
		},
		{
			name:        "one missing, named",
			want:        []string{"p1", "p2"},
			have:        []string{"p1", "p3"},
			wantMissing: []string{"p2"},
		},
		{
			name:        "duplicate on the origin side needs a match each time",
			want:        []string{"p1", "p1"},
			have:        []string{"p1"},
			wantMissing: []string{"p1"},
		},
		{
			name: "duplicates matched",
			want: []string{"p1", "p1"},
			have: []string{"p1", "p1", "p2"},
		},
		{
			name:        "empty origin is trivially contained",
			want:        nil,
			have:        []string{"p1"},
			wantMissing: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := patchIDsMissingFrom(tt.want, tt.have)
			if strings.Join(got, ",") != strings.Join(tt.wantMissing, ",") {
				t.Errorf("patchIDsMissingFrom = %v, want %v", got, tt.wantMissing)
			}
		})
	}
}

func TestRecoverDivergedPush_LeaseFails(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		patchIDs: map[[2]string]string{
			{"base", "origSHA"}:  "samepatch",
			{"base", "localSHA"}: "samepatch",
		},
		patchIDLists: map[[2]string][]string{
			{"base", "origSHA"}:  {"p1"},
			{"base", "localSHA"}: {"p1"},
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
	t.Parallel()
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

// TestRecoverDivergedPush_RealRepoReworkRedispatch is the end-to-end form of
// the gt-i0z3 bug against real git: the reused branch is rebased *and* carries
// a new fix commit, which is what mol-polecat-work's branch-reuse step does on
// every redispatch. The two ranges are not patch-identical by design, so the
// pure-rebase-only check called it "real divergence" and raised the possible
// work-loss alarm on work that was never at risk. Recovery must land all three
// commits' content.
func TestRecoverDivergedPush_RealRepoReworkRedispatch(t *testing.T) {
	t.Parallel()
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

	// First dispatch: two commits, pushed — this is the state origin holds
	// when the branch comes back for rework.
	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "feature.txt", "feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature work")
	writeRepoFile(t, seed, "feature-two.txt", "more feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "more feature work")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	// main advances while the MR waits in the queue.
	testRunGit(t, seed, "checkout", "main")
	writeRepoFile(t, seed, "main-new.txt", "advance\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "advance main")
	testRunGit(t, seed, "push", "origin", "main")

	// Second dispatch: reuse the branch, rebase onto origin/main, then add the
	// targeted fix commit the rework produced.
	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "-b", "feature", "origin/feature")
	testRunGit(t, work, "fetch", "origin")
	testRunGit(t, work, "rebase", "origin/main")
	writeRepoFile(t, work, "rework-fix.txt", "the review fix\n")
	testRunGit(t, work, "add", ".")
	testRunGit(t, work, "commit", "-m", "rework: address review feedback")

	g := gitpkg.NewGit(work)
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward after rebase + rework commit")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery on the rework-redispatch path, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "rework-redispatch") {
		t.Errorf("diagnosis = %q, want it to name the rework case", diagnosis)
	}

	// Every commit's content must be on origin: the two origin already had,
	// plus the rework fix, on top of the advanced main.
	verify := filepath.Join(tmp, "verify")
	testRunGit(t, tmp, "clone", "--branch", "feature", remote, verify)
	for _, name := range []string{"main-new.txt", "feature.txt", "feature-two.txt", "rework-fix.txt"} {
		if _, statErr := os.Stat(filepath.Join(verify, name)); statErr != nil {
			t.Errorf("%s missing on origin/feature after recovery: %v", name, statErr)
		}
	}
}

// TestRecoverDivergedPush_RealRepoRefusesGenuineDivergence guards the safety
// side: when origin's tip is real, different work (not the same content
// rebased), recovery must refuse and leave origin untouched rather than
// clobber it.
func TestRecoverDivergedPush_RealRepoRefusesGenuineDivergence(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}

	testRunGit(t, work, "fetch", "origin")
	if got := gitpkgRev(t, work, "origin/feature"); got != otherHead {
		t.Errorf("origin/feature was modified: got %s, want %s (untouched)", got, otherHead)
	}
}

// TestRecoverDivergedPush_RealRepoMergeCommitContentRefuses guards the gap
// gt-5wp5 found: a merge commit can carry content of its own — whatever was
// added while resolving it, on top of what either parent already
// contributed — that no individual commit's diff carries. A default rebase
// drops merge commits, replaying only their non-merge ancestors, so a branch
// rebased after landing such a merge loses that content entirely. The
// per-commit comparison must catch this and refuse, not force-push local's
// copy (missing the content) over origin's (which still has it).
func TestRecoverDivergedPush_RealRepoMergeCommitContentRefuses(t *testing.T) {
	t.Parallel()
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

	// Two branches off the same point, touching different files so both
	// replay cleanly after a later rebase — the content this test is about
	// comes from the merge commit itself, not from a replay conflict.
	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "a.txt", "feature\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature: add a.txt")

	testRunGit(t, seed, "checkout", "main")
	testRunGit(t, seed, "checkout", "-b", "topic")
	writeRepoFile(t, seed, "b.txt", "topic\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "topic: add b.txt")

	// Merge topic into feature, then add content in the merge commit itself
	// — a conflict resolution lands the same way. c.txt exists only on this
	// commit, attached to neither parent's own diff.
	testRunGit(t, seed, "checkout", "feature")
	testRunGit(t, seed, "merge", "--no-ff", "--no-commit", "topic")
	writeRepoFile(t, seed, "c.txt", "resolved during the merge\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "Merge topic into feature, plus fixup")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	// main advances while the MR sits in the queue.
	testRunGit(t, seed, "checkout", "main")
	writeRepoFile(t, seed, "main-new.txt", "advance\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "advance main")
	testRunGit(t, seed, "push", "origin", "main")

	// Second dispatch: fresh checkout of the reused branch, rebased onto
	// origin/main per the formula's branch-reuse step. A default rebase drops
	// the merge commit and replays its non-merge ancestors individually —
	// a.txt and b.txt both come back, but c.txt (the merge's own content)
	// does not.
	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "-b", "feature", "origin/feature")
	testRunGit(t, work, "fetch", "origin")
	testRunGit(t, work, "rebase", "origin/main")

	if _, statErr := os.Stat(filepath.Join(work, "c.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("precondition failed: rebase should have dropped c.txt, stat err: %v", statErr)
	}

	g := gitpkg.NewGit(work)
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward after rebase")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: the rebase dropped the merge commit's own content (c.txt)")
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}

	// origin/feature must still carry the merge's content untouched.
	verify := filepath.Join(tmp, "verify")
	testRunGit(t, tmp, "clone", "--branch", "feature", remote, verify)
	if _, statErr := os.Stat(filepath.Join(verify, "c.txt")); statErr != nil {
		t.Errorf("c.txt missing on origin/feature after recovery attempt: %v", statErr)
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
