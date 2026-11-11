package done

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
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

	// summaries backs FirstParentCommitSummaries: the author and subject of
	// each commit in a range, keyed by the same [base, head] pair.
	summaries    map[[2]string][]gitpkg.CommitSummary
	summariesErr error

	// fullSummaries backs FullRangeCommitSummaries: every commit in a range,
	// merged-in ones included, keyed by the same [base, head] pair.
	fullSummaries    map[[2]string][]gitpkg.CommitSummary
	fullSummariesErr error

	// diffPaths backs DiffNameOnly, keyed by the [base, head] pair it is asked
	// about: the old-to-new base pair for the paths main moved, and [sha^, sha]
	// for a commit's own changed paths. Unset pairs have no paths.
	diffPaths map[[2]string][]string
	pathErrs  map[[2]string]error

	// excluded backs PatchIDExcluding, keyed by the same [base, head] pair. An
	// unset pair is the empty id: nothing left outside the excluded paths.
	excluded     map[[2]string]string
	excludedErrs map[[2]string]error

	// summaryErrs fails FirstParentCommitSummaries for one [base, head] pair.
	summaryErrs map[[2]string]error

	leaseErr   error
	leaseCalls int
	leaseArgs  []string

	fetchErr   error
	fetchCalls int

	// ownFetches records each refspec fetched by name, as "remote refspec".
	ownFetches  []string
	ownFetchErr error
}

func (f *fakeDivergedPushGit) Fetch(remote string) error {
	f.fetchCalls++
	return f.fetchErr
}

func (f *fakeDivergedPushGit) FetchRefspecWithTimeout(remote, refspec string, _ time.Duration) error {
	f.ownFetches = append(f.ownFetches, remote+" "+refspec)
	return f.ownFetchErr
}

// patchIdenticalDivergence is a fake whose origin tip and local HEAD differ by
// a rebase only, so recoverDivergedPush recovers unless a fetch fails first.
func patchIdenticalDivergence() *fakeDivergedPushGit {
	return &fakeDivergedPushGit{
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
	// The commit behind each id is the summary at the same index when the test
	// gave one, so a refusal can be checked against a real sha and subject.
	summaries := f.summaries[[2]string{base, head}]
	pairs := make([]gitpkg.PatchIDCommit, len(ids))
	for i, id := range ids {
		commit := fmt.Sprintf("commit-%d", i)
		if i < len(summaries) {
			commit = summaries[i].SHA
		}
		pairs[i] = gitpkg.PatchIDCommit{PatchID: id, Commit: commit}
	}
	return pairs, nil
}

func (f *fakeDivergedPushGit) FirstParentCommitSummaries(base, head string) ([]gitpkg.CommitSummary, error) {
	if f.summariesErr != nil {
		return nil, f.summariesErr
	}
	if err := f.summaryErrs[[2]string{base, head}]; err != nil {
		return nil, err
	}
	return f.summaries[[2]string{base, head}], nil
}

func (f *fakeDivergedPushGit) DiffNameOnly(base, head string) ([]string, error) {
	if err := f.pathErrs[[2]string{base, head}]; err != nil {
		return nil, err
	}
	return f.diffPaths[[2]string{base, head}], nil
}

func (f *fakeDivergedPushGit) PatchIDExcluding(base, head string, _ []string) (string, error) {
	if err := f.excludedErrs[[2]string{base, head}]; err != nil {
		return "", err
	}
	return f.excluded[[2]string{base, head}], nil
}

func (f *fakeDivergedPushGit) FullRangeCommitSummaries(base, head string) ([]gitpkg.CommitSummary, error) {
	if f.fullSummariesErr != nil {
		return nil, f.fullSummariesErr
	}
	return f.fullSummaries[[2]string{base, head}], nil
}

func (f *fakeDivergedPushGit) PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error {
	f.leaseCalls++
	f.leaseArgs = []string{remote, refspec, branchRef, expectedSHA}
	return f.leaseErr
}

func TestRecoverDivergedPush_FetchFailsAborts(t *testing.T) {
	t.Parallel()
	f := &fakeDivergedPushGit{fetchErr: errors.New("network unreachable")}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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

// The own branch is fetched by name before origin/<branch> is read: the bare
// fetch no longer refreshes it in a polecat clone (the clone pins main only).
func TestRecoverDivergedPush_RefreshesOwnBranchByName(t *testing.T) {
	t.Parallel()
	f := patchIdenticalDivergence()
	recovered, _, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Error("a branch origin still holds must be read and recovered")
	}
	want := []string{"origin +refs/heads/feature:refs/remotes/origin/feature"}
	if strings.Join(f.ownFetches, "|") != strings.Join(want, "|") {
		t.Errorf("fetched by name = %v, want %v", f.ownFetches, want)
	}
}

func TestRecoverDivergedPush_OwnBranchFetchFailsAborts(t *testing.T) {
	t.Parallel()
	f := patchIdenticalDivergence()
	f.ownFetchErr = errors.New("network unreachable")
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
	if recovered {
		t.Error("must not recover on a stale origin/feature when its fetch fails")
	}
	if diagnosis != "" {
		t.Errorf("diagnosis = %q, want empty (comparison never ran)", diagnosis)
	}
	if err == nil || !strings.Contains(err.Error(), "fetch origin/feature before divergence check") {
		t.Fatalf("err = %v, want the own-branch fetch failure", err)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

// A branch origin landed and deleted is not a fetch failure: the own-branch
// fetch reports a missing ref, and recoverDivergedPush takes the no-ref path.
// Without that routing the git error surfaces as a fetch failure instead.
func TestRecoverDivergedPush_OwnBranchDeletedOnOriginIsNoRef(t *testing.T) {
	t.Parallel()
	f := patchIdenticalDivergence()
	f.ownFetchErr = &gitpkg.GitError{Command: "fetch", Stderr: "fatal: couldn't find remote ref refs/heads/feature"}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
	if recovered {
		t.Error("must not recover when origin deleted the branch")
	}
	if diagnosis != "" {
		t.Errorf("diagnosis = %q, want empty (comparison never ran)", diagnosis)
	}
	if err == nil || !strings.Contains(err.Error(), "origin/feature has no ref") {
		t.Fatalf("err = %v, want the no-ref outcome", err)
	}
	if strings.Contains(err.Error(), "before divergence check") {
		t.Errorf("err = %v, a deleted branch must not be reported as a fetch failure", err)
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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

// conflictRebaseFake is origin holding one feature commit o1, and local holding
// its rebase l1: same author and subject, same changed path, which main also
// moved, and nothing outside that path. Each row changes one thing from it.
func conflictRebaseFake() *fakeDivergedPushGit {
	return &fakeDivergedPushGit{
		revs: map[string]string{
			"origin/feature": "origSHA",
			"HEAD":           "localSHA",
		},
		mergeBases: map[[2]string]string{
			{"origin/main", "origSHA"}:  "base1",
			{"origin/main", "localSHA"}: "base2",
		},
		patchIDLists: map[[2]string][]string{
			{"base1", "origSHA"}:  {"p-origin"},
			{"base2", "localSHA"}: {"p-local"},
		},
		summaries: map[[2]string][]gitpkg.CommitSummary{
			{"base1", "origSHA"}:  {{SHA: "o1", Email: "a@example.com", Subject: "feature work"}},
			{"base2", "localSHA"}: {{SHA: "l1", Email: "a@example.com", Subject: "feature work"}},
		},
		diffPaths: map[[2]string][]string{
			{"base1", "base2"}: {"allow.txt"},
			{"o1^", "o1"}:      {"allow.txt"},
			{"l1^", "l1"}:      {"allow.txt"},
		},
		excluded: map[[2]string]string{
			{"o1^", "o1"}: "",
			{"l1^", "l1"}: "",
		},
	}
}

// TestRecoverDivergedPush_ConflictResolvedRebase is the gt-d8rpw0 decision: a
// rebase that resolved a conflict in a path main moved is recovered under a
// lease on origin's tip, and every other difference is real divergence.
func TestRecoverDivergedPush_ConflictResolvedRebase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		mutate        func(f *fakeDivergedPushGit)
		wantRecovered bool
		wantErr       bool
	}{
		{
			name:          "resolved line in a path main moved is recovered",
			mutate:        func(*fakeDivergedPushGit) {},
			wantRecovered: true,
		},
		{
			name: "origin holds a second commit local lacks",
			mutate: func(f *fakeDivergedPushGit) {
				f.patchIDLists[[2]string{"base1", "origSHA"}] = []string{"p-origin", "p-extra"}
				f.summaries[[2]string{"base1", "origSHA"}] = append(f.summaries[[2]string{"base1", "origSHA"}],
					gitpkg.CommitSummary{SHA: "o2", Email: "a@example.com", Subject: "second"})
				f.diffPaths[[2]string{"o2^", "o2"}] = []string{"allow.txt"}
				f.excluded[[2]string{"o2^", "o2"}] = ""
			},
		},
		{
			name: "local counterpart has a different author",
			mutate: func(f *fakeDivergedPushGit) {
				f.summaries[[2]string{"base2", "localSHA"}][0].Email = "b@example.com"
			},
		},
		{
			name: "local counterpart has a different subject",
			mutate: func(f *fakeDivergedPushGit) {
				f.summaries[[2]string{"base2", "localSHA"}][0].Subject = "rewritten"
			},
		},
		{
			name: "local counterpart differs outside the paths main moved",
			mutate: func(f *fakeDivergedPushGit) {
				f.excluded[[2]string{"l1^", "l1"}] = "other-content"
			},
		},
		{
			name: "origin commit changes no path",
			mutate: func(f *fakeDivergedPushGit) {
				delete(f.diffPaths, [2]string{"o1^", "o1"})
			},
		},
		{
			name: "origin commit has no recorded summary",
			mutate: func(f *fakeDivergedPushGit) {
				delete(f.summaries, [2]string{"base1", "origSHA"})
			},
		},
		{
			name: "one local commit cannot cover two origin commits",
			mutate: func(f *fakeDivergedPushGit) {
				f.patchIDLists[[2]string{"base1", "origSHA"}] = []string{"p-origin", "p-extra"}
				f.summaries[[2]string{"base1", "origSHA"}] = []gitpkg.CommitSummary{
					{SHA: "o1", Email: "a@example.com", Subject: "feature work"},
					{SHA: "o2", Email: "a@example.com", Subject: "feature work"},
				}
				f.diffPaths[[2]string{"o2^", "o2"}] = []string{"allow.txt"}
				f.excluded[[2]string{"o2^", "o2"}] = ""
				f.patchIDLists[[2]string{"base2", "localSHA"}] = []string{"p-local", "p-local2"}
				f.summaries[[2]string{"base2", "localSHA"}] = []gitpkg.CommitSummary{
					{SHA: "l1", Email: "a@example.com", Subject: "feature work"},
					{SHA: "l2", Email: "a@example.com", Subject: "other"},
				}
				f.diffPaths[[2]string{"l2^", "l2"}] = []string{"allow.txt"}
				f.excluded[[2]string{"l2^", "l2"}] = ""
			},
		},
		{
			name: "listing the paths main moved fails",
			mutate: func(f *fakeDivergedPushGit) {
				f.pathErrs = map[[2]string]error{{"base1", "base2"}: errors.New("diff failed")}
			},
			wantErr: true,
		},
		{
			name: "listing a local commit's paths fails",
			mutate: func(f *fakeDivergedPushGit) {
				f.pathErrs = map[[2]string]error{{"l1^", "l1"}: errors.New("diff failed")}
			},
			wantErr: true,
		},
		{
			name: "listing an origin commit's paths fails",
			mutate: func(f *fakeDivergedPushGit) {
				f.pathErrs = map[[2]string]error{{"o1^", "o1"}: errors.New("diff failed")}
			},
			wantErr: true,
		},
		{
			name: "diff outside the moved paths fails",
			mutate: func(f *fakeDivergedPushGit) {
				f.excludedErrs = map[[2]string]error{{"o1^", "o1"}: errors.New("patch-id failed")}
			},
			wantErr: true,
		},
		{
			name: "local range summaries fail",
			mutate: func(f *fakeDivergedPushGit) {
				f.summaryErrs = map[[2]string]error{{"base2", "localSHA"}: errors.New("log failed")}
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := conflictRebaseFake()
			tc.mutate(f)
			recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got recovered=%v diagnosis=%q", recovered, diagnosis)
				}
				if recovered || f.leaseCalls != 0 {
					t.Errorf("an error must not push: recovered=%v lease calls=%d", recovered, f.leaseCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if recovered != tc.wantRecovered {
				t.Fatalf("recovered = %v, want %v (diagnosis %q)", recovered, tc.wantRecovered, diagnosis)
			}
			if !tc.wantRecovered {
				if f.leaseCalls != 0 {
					t.Errorf("lease push must not run on real divergence, got %d calls", f.leaseCalls)
				}
				if !strings.Contains(diagnosis, "real divergence") {
					t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
				}
				return
			}
			if !strings.Contains(diagnosis, "rebase over a moved main with a conflict resolved") {
				t.Errorf("diagnosis = %q, want the conflict-resolved rebase named", diagnosis)
			}
			wantArgs := []string{"origin", "feature:feature", "feature", "origSHA"}
			if !slices.Equal(f.leaseArgs, wantArgs) {
				t.Errorf("lease args = %v, want %v: the push must lease over origin's old tip", f.leaseArgs, wantArgs)
			}
		})
	}
}

func TestMissingCommits(t *testing.T) {
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
			var want []gitpkg.PatchIDCommit
			for i, id := range tt.want {
				want = append(want, gitpkg.PatchIDCommit{PatchID: id, Commit: fmt.Sprintf("commit-%d", i)})
			}
			var got []string
			for _, p := range missingCommits(want, tt.have) {
				got = append(got, p.PatchID)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantMissing, ",") {
				t.Errorf("missingCommits = %v, want %v", got, tt.wantMissing)
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
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
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

const (
	reworkTestRejected = "aaaa1111bbbb2222cccc3333dddd4444eeee5555"
	reworkTestLocal    = "ffff0000111122223333444455556666777788889"
	reworkTestSeat     = "capable"
	reworkTestEmail    = "seat@example.com"
)

// reworkFakeGit is a divergence where origin's tip is one commit, written by
// author at email, and local's one commit carries a different patch-id: a
// rework that changed the content. The origin commit's sha is originTip itself.
func reworkFakeGit(originTip, author, email string) *fakeDivergedPushGit {
	commits := []gitpkg.CommitSummary{{SHA: originTip, Author: author, Email: email, Subject: "feat: the rejected submission"}}
	return &fakeDivergedPushGit{
		revs: map[string]string{"origin/feature": originTip, "HEAD": reworkTestLocal},
		patchIDLists: map[[2]string][]string{
			{"base", originTip}:       {"p-origin"},
			{"base", reworkTestLocal}: {"p-local"},
		},
		summaries:     map[[2]string][]gitpkg.CommitSummary{{"base", originTip}: commits},
		fullSummaries: map[[2]string][]gitpkg.CommitSummary{{"base", originTip}: commits},
	}
}

func TestRecoverDivergedPush_ReworkOfRejectedHeadPushesUnderLease(t *testing.T) {
	t.Parallel()
	f := reworkFakeGit(reworkTestRejected, reworkTestSeat, reworkTestEmail)
	rework := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", rework)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("rework of the rejected head must push; diagnosis: %s", diagnosis)
	}
	if !strings.Contains(diagnosis, "rework of the rejected submission") {
		t.Errorf("diagnosis = %q, want the rework reason", diagnosis)
	}
	if f.leaseCalls != 1 {
		t.Fatalf("lease calls = %d, want 1", f.leaseCalls)
	}
	if got := f.leaseArgs[3]; got != reworkTestRejected {
		t.Errorf("lease pinned to %q, want the observed origin tip %q", got, reworkTestRejected)
	}
}

// A stale worktree whose HEAD is an ancestor of the rejected head carries
// nothing origin lacks. Its lease push would replace the rejected submission
// with less of it, so the rework allowance must not apply.
func TestRecoverDivergedPushReworkRefusesStaleLocal(t *testing.T) {
	t.Parallel()
	const stale = "cccc9999dddd8888eeee7777ffff6666aaaa5555"
	f := reworkFakeGit(reworkTestRejected, reworkTestSeat, reworkTestEmail)
	f.revs["HEAD"] = stale
	f.mergeBases = map[[2]string]string{{reworkTestRejected, stale}: stale}
	f.patchIDLists = map[[2]string][]string{
		{"base", reworkTestRejected}: {"p-old", "p-origin"},
		{"base", stale}:              {"p-old"},
	}
	commits := []gitpkg.CommitSummary{
		{SHA: "old", Author: reworkTestSeat, Email: reworkTestEmail, Subject: "feat: earlier submission"},
		{SHA: reworkTestRejected, Author: reworkTestSeat, Email: reworkTestEmail, Subject: "feat: the rejected submission"},
	}
	f.summaries = map[[2]string][]gitpkg.CommitSummary{{"base", reworkTestRejected}: commits}
	f.fullSummaries = map[[2]string][]gitpkg.CommitSummary{{"base", reworkTestRejected}: commits}
	rework := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", rework)
	if recovered {
		t.Fatal("a stale local head behind the rejected head must not overwrite it under the rework lease")
	}
	if err != nil {
		t.Fatalf("refusal is a diagnosis, not an error: %v", err)
	}
	if strings.Contains(diagnosis, "carries the rework") {
		t.Errorf("diagnosis = %q, must not claim the stale local carries the rework", diagnosis)
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want the divergence refusal", diagnosis)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

func TestRecoverDivergedPush_ReworkWithForeignCommitRefuses(t *testing.T) {
	t.Parallel()
	f := reworkFakeGit(reworkTestRejected, "Test Operator", reworkTestEmail)
	rework := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", rework)
	if recovered {
		t.Fatal("origin holding another author's commit must not be overwritten")
	}
	if err != nil {
		t.Fatalf("refusal is a diagnosis, not an error: %v", err)
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want the divergence refusal", diagnosis)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

// The rejected head is a seat-authored merge: its first-parent chain (the merge
// and its first parent) is all the seat's, and only the full range shows the
// other author's commit on the second parent. Overwriting origin would drop it.
func TestRecoverDivergedPush_ReworkMergeWithForeignSecondParentRefuses(t *testing.T) {
	t.Parallel()
	f := reworkFakeGit(reworkTestRejected, reworkTestSeat, reworkTestEmail)
	f.summaries[[2]string{"base", reworkTestRejected}] = []gitpkg.CommitSummary{
		{SHA: reworkTestRejected, Author: reworkTestSeat, Email: reworkTestEmail, Subject: "Merge branch 'side'"},
		{SHA: "p1", Author: reworkTestSeat, Email: reworkTestEmail, Subject: "feat: the first parent"},
	}
	f.fullSummaries[[2]string{"base", reworkTestRejected}] = []gitpkg.CommitSummary{
		{SHA: reworkTestRejected, Author: reworkTestSeat, Email: reworkTestEmail, Subject: "Merge branch 'side'"},
		{SHA: "s1", Author: "Test Operator", Email: reworkTestEmail, Subject: "fix: the foreign commit"},
		{SHA: "p1", Author: reworkTestSeat, Email: reworkTestEmail, Subject: "feat: the first parent"},
	}
	rework := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", rework)
	if recovered {
		t.Fatal("a foreign commit reachable only through a merge's second parent must not be overwritten")
	}
	if err != nil {
		t.Fatalf("refusal is a diagnosis, not an error: %v", err)
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want the divergence refusal", diagnosis)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

func TestRecoverDivergedPush_ReworkFullRangeErrorAborts(t *testing.T) {
	t.Parallel()
	f := reworkFakeGit(reworkTestRejected, reworkTestSeat, reworkTestEmail)
	f.fullSummariesErr = errors.New("log failed")
	rework := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}
	recovered, _, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", rework)
	if recovered || err == nil || !strings.Contains(err.Error(), "full commit summaries") {
		t.Errorf("recovered=%v err=%v, want an abort naming the full-range listing", recovered, err)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

func TestRecoverDivergedPush_ReworkAfterOriginMovedRefuses(t *testing.T) {
	t.Parallel()
	const moved = "9999aaaabbbbccccddddeeeeffff000011112222"
	f := reworkFakeGit(moved, reworkTestSeat, reworkTestEmail)
	rework := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}
	recovered, diagnosis, err := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", rework)
	if recovered {
		t.Fatal("origin's tip moved after the rejection, so it is not the rejected submission")
	}
	if err != nil {
		t.Fatalf("refusal is a diagnosis, not an error: %v", err)
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want the divergence refusal", diagnosis)
	}
	if f.leaseCalls != 0 {
		t.Errorf("lease push must not run, got %d calls", f.leaseCalls)
	}
}

func TestRecoverDivergedPush_RefusalNamesPatchIDCommits(t *testing.T) {
	t.Parallel()
	f := reworkFakeGit(reworkTestRejected, "Test Operator", reworkTestEmail)
	_, diagnosis, _ := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
	if !strings.Contains(diagnosis, "the ids below are patch-ids") {
		t.Errorf("diagnosis = %q, want it to say the ids are patch-ids", diagnosis)
	}
	want := "patch-id p-origin = commit aaaa1111 feat: the rejected submission"
	if !strings.Contains(diagnosis, want) {
		t.Errorf("diagnosis = %q, want the mapping line %q", diagnosis, want)
	}
}

func TestRecoverDivergedPush_RefusalSaysWhenACommitHasNoSubject(t *testing.T) {
	t.Parallel()
	f := reworkFakeGit(reworkTestRejected, "Test Operator", reworkTestEmail)
	// The patch-id walk sees origin's commit, the summary walk does not.
	f.summaries = nil
	_, diagnosis, _ := recoverDivergedPush(f, "origin", "feature:feature", "feature", "origin/main", reworkPush{})
	want := "patch-id p-origin = commit commit-0 (no subject: the commit is not in origin's summary listing)"
	if !strings.Contains(diagnosis, want) {
		t.Errorf("diagnosis = %q, want the explicit line %q", diagnosis, want)
	}
}

func TestReworkPushAllows(t *testing.T) {
	t.Parallel()
	const (
		localAddr = reworkTestSeat + "@gastown.local"
		legacy    = "host@example.com"
	)
	seatOnly := []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: reworkTestSeat, Email: reworkTestEmail}}
	seat := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}
	withLocal := reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail, seatLocalEmail: localAddr, legacyEmail: legacy}
	tests := []struct {
		name   string
		rework reworkPush
		origin string
		commit []gitpkg.CommitSummary
		want   bool
	}{
		{name: "zero value never allows", origin: reworkTestRejected, commit: seatOnly},
		{name: "seat commits over the rejected head", rework: seat, origin: reworkTestRejected, commit: seatOnly, want: true},
		{name: "abbreviated rejected head matches", rework: reworkPush{rejectedHead: reworkTestRejected[:8], seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail}, origin: reworkTestRejected, commit: seatOnly, want: true},
		{name: "origin tip is not the rejected head", rework: seat, origin: reworkTestLocal, commit: seatOnly},
		{name: "foreign author on origin", rework: seat, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: "Test Operator", Email: reworkTestEmail}}},
		{name: "seat name from another mailbox", rework: seat, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: reworkTestSeat, Email: "other@example.com"}}},
		{name: "one foreign commit among seat commits", rework: seat, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: "c1", Author: reworkTestSeat, Email: reworkTestEmail}, {SHA: reworkTestRejected, Author: "Test Operator", Email: reworkTestEmail}}},
		{name: "empty origin range is not all seat's", rework: seat, origin: reworkTestRejected},
		{name: "no seat author", rework: reworkPush{rejectedHead: reworkTestRejected, seatEmail: reworkTestEmail}, origin: reworkTestRejected, commit: seatOnly},
		{name: "no seat email fails closed", rework: reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat}, origin: reworkTestRejected, commit: seatOnly},
		{name: "all under the seat's gastown.local address", rework: withLocal, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: reworkTestSeat, Email: localAddr}}, want: true},
		{name: "mix of legacy global email and gastown.local under the right name", rework: withLocal, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: "c1", Author: reworkTestSeat, Email: legacy}, {SHA: reworkTestRejected, Author: reworkTestSeat, Email: localAddr}}, want: true},
		{name: "legacy global email under the right name", rework: withLocal, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: reworkTestSeat, Email: legacy}}, want: true},
		{name: "another name under the gastown.local address", rework: withLocal, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: "Test Operator", Email: localAddr}}},
		{name: "another name under the legacy global email", rework: withLocal, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: "Test Operator", Email: legacy}}},
		{name: "a gastown.local address that is not this seat's", rework: withLocal, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: reworkTestSeat, Email: "other@gastown.local"}}},
		{name: "gastown.local address without the local address configured", rework: seat, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: reworkTestSeat, Email: localAddr}}},
		{name: "empty legacy email never matches an empty author email", rework: reworkPush{rejectedHead: reworkTestRejected, seatAuthor: reworkTestSeat, seatEmail: reworkTestEmail, seatLocalEmail: localAddr}, origin: reworkTestRejected, commit: []gitpkg.CommitSummary{{SHA: reworkTestRejected, Author: reworkTestSeat, Email: ""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rework.allows(tt.origin, tt.commit); got != tt.want {
				t.Errorf("allows = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReworkPushForNilIssueIsEmpty(t *testing.T) {
	t.Parallel()
	if got := reworkPushFor(nil, reworkTestSeat, reworkTestEmail, "", ""); got != (reworkPush{}) {
		t.Errorf("reworkPushFor(nil) = %+v, want the zero value", got)
	}
}

func TestReworkPushForNoSeatEmailIsEmpty(t *testing.T) {
	t.Parallel()
	issue := reworkBead([]string{land.LabelRework}, reworkStandDownNote("", "feature", reworkTestRejected))
	if got := reworkPushFor(issue, reworkTestSeat, "", reworkTestSeat+"@gastown.local", "host@example.com"); got != (reworkPush{}) {
		t.Errorf("reworkPushFor with no seat email = %+v, want the zero value", got)
	}
}

// reworkLocalCarriesCommits is the rework allowance's last check: a local head
// at or behind origin's tip carries no rework, so only a local head holding a
// commit origin lacks may lease over origin.
func TestReworkLocalCarriesCommits(t *testing.T) {
	t.Parallel()
	const (
		origin = "aaaa1111bbbb2222cccc3333dddd4444eeee5555"
		local  = "ffff0000111122223333444455556666777788889"
		stale  = "cccc9999dddd8888eeee7777ffff6666aaaa5555"
	)
	tests := []struct {
		name     string
		localSHA string
		base     string
		mergeErr error
		want     bool
		wantErr  bool
	}{
		{
			name:     "equal heads carry nothing and skip the merge-base",
			localSHA: origin,
			mergeErr: errors.New("merge-base must not run for equal heads"),
			want:     false,
		},
		{
			name:     "local at a merge-base behind origin is stale",
			localSHA: stale,
			base:     stale,
			want:     false,
		},
		{
			name:     "local holding a commit origin lacks carries the rework",
			localSHA: local,
			base:     origin,
			want:     true,
		},
		{
			name:     "empty merge-base fails closed",
			localSHA: local,
			base:     "",
			want:     false,
		},
		{
			name:     "merge-base error is returned",
			localSHA: local,
			mergeErr: errors.New("object missing"),
			want:     false,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeDivergedPushGit{
				mergeBases:   map[[2]string]string{{origin, tt.localSHA}: tt.base},
				mergeBaseErr: tt.mergeErr,
			}
			got, err := reworkLocalCarriesCommits(f, origin, tt.localSHA)
			if (err != nil) != tt.wantErr {
				t.Fatalf("reworkLocalCarriesCommits error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("reworkLocalCarriesCommits = %v, want %v", got, tt.want)
			}
		})
	}
}
