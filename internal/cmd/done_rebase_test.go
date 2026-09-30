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
