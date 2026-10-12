package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeLocalPruneRepo is one rig's repo in memory: the branches it holds, the
// ones a live checkout holds, the ones merged into its base ref, and the
// failures a test wants the sweep to hit. It records what the sweep deleted.
type fakeLocalPruneRepo struct {
	branches    []string
	merged      []string
	held        []string
	baseRef     string
	baseErr     error
	listErr     error
	worktreeErr error
	ancestryErr map[string]error
	deleteErr   map[string]error
	deleted     []string
}

func (f *fakeLocalPruneRepo) LocalPolecatBranches() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return slices.Clone(f.branches), nil
}

func (f *fakeLocalPruneRepo) BaseRef() (string, error) {
	if f.baseErr != nil {
		return "", f.baseErr
	}
	if f.baseRef == "" {
		return "origin/main", nil
	}
	return f.baseRef, nil
}

func (f *fakeLocalPruneRepo) IsAncestor(ancestor, descendant string) (bool, error) {
	if err := f.ancestryErr[ancestor]; err != nil {
		return false, err
	}
	return slices.Contains(f.merged, ancestor), nil
}

func (f *fakeLocalPruneRepo) HeldBranches() (map[string]bool, error) {
	if f.worktreeErr != nil {
		return nil, f.worktreeErr
	}
	out := make(map[string]bool, len(f.held))
	for _, b := range f.held {
		out[b] = true
	}
	return out, nil
}

func (f *fakeLocalPruneRepo) DeleteBranch(name string) error {
	if err := f.deleteErr[name]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, name)
	return nil
}

// TestPruneLocalPolecatBranchesSeparatesMergedCheckedOutAndUnmerged pins the
// three buckets one rig's sweep sorts its branches into, and that the dry run
// reaches the same decisions while deleting nothing.
func TestPruneLocalPolecatBranchesSeparatesMergedCheckedOutAndUnmerged(t *testing.T) {
	t.Parallel()
	for _, apply := range []bool{false, true} {
		repo := &fakeLocalPruneRepo{
			branches: []string{"polecat/b/gt-2+2", "polecat/a/gt-1+1", "polecat/c/gt-3+3", "polecat/d/gt-4+4"},
			merged:   []string{"polecat/a/gt-1+1", "polecat/b/gt-2+2"},
			held:     []string{"polecat/b/gt-2+2"},
		}

		res := pruneLocalPolecatBranches(repo, apply)

		wantDeleted := []string{"polecat/a/gt-1+1"}
		wantKept := []string{"polecat/c/gt-3+3", "polecat/d/gt-4+4"}
		if !slices.Equal(res.Deleted, wantDeleted) {
			t.Errorf("apply=%v: deleted %v, want %v", apply, res.Deleted, wantDeleted)
		}
		if !slices.Equal(res.Held, []string{"polecat/b/gt-2+2"}) {
			t.Errorf("apply=%v: checked out %v, want the merged branch a worktree holds", apply, res.Held)
		}
		if !slices.Equal(res.Unmerged, wantKept) {
			t.Errorf("apply=%v: unmerged %v, want %v", apply, res.Unmerged, wantKept)
		}
		if res.SkippedReason != "" || len(res.Failures) != 0 {
			t.Errorf("apply=%v: skipped %q failures %v, want neither", apply, res.SkippedReason, res.Failures)
		}
		if apply {
			if !slices.Equal(repo.deleted, wantDeleted) {
				t.Errorf("applied run deleted %v, want %v", repo.deleted, wantDeleted)
			}
		} else if len(repo.deleted) != 0 {
			t.Errorf("dry run deleted %v, want nothing", repo.deleted)
		}
	}
}

// TestPruneLocalPolecatBranchesKeepsCheckedOutBranchEvenWhenMerged pins the
// worktree rule on its own: a branch whose commits are on the base ref is
// still live work while a worktree has it checked out.
func TestPruneLocalPolecatBranchesKeepsCheckedOutBranchEvenWhenMerged(t *testing.T) {
	t.Parallel()
	repo := &fakeLocalPruneRepo{
		branches: []string{"polecat/a/gt-1+1"},
		merged:   []string{"polecat/a/gt-1+1"},
		held:     []string{"polecat/a/gt-1+1"},
	}

	res := pruneLocalPolecatBranches(repo, true)

	if len(res.Deleted) != 0 || len(repo.deleted) != 0 {
		t.Fatalf("deleted %v (repo %v), want the checked-out branch kept", res.Deleted, repo.deleted)
	}
	if !slices.Equal(res.Held, []string{"polecat/a/gt-1+1"}) {
		t.Errorf("checked out %v, want the branch a worktree holds", res.Held)
	}
}

// TestPruneLocalPolecatBranchesSkipsRigWhenItCannotReadTheRepo pins fail
// closed: with no base ref (or no worktree list) nothing in that rig is even
// classified, let alone deleted.
func TestPruneLocalPolecatBranchesSkipsRigWhenItCannotReadTheRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		repo *fakeLocalPruneRepo
	}{
		{name: "no base ref", repo: &fakeLocalPruneRepo{
			branches: []string{"polecat/a/gt-1+1"},
			merged:   []string{"polecat/a/gt-1+1"},
			baseErr:  errors.New("origin/main does not exist"),
		}},
		{name: "branch listing fails", repo: &fakeLocalPruneRepo{
			listErr: errors.New("not a git repository"),
		}},
		{name: "worktree listing fails", repo: &fakeLocalPruneRepo{
			branches:    []string{"polecat/a/gt-1+1"},
			merged:      []string{"polecat/a/gt-1+1"},
			worktreeErr: errors.New("worktree list failed"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := pruneLocalPolecatBranches(tc.repo, true)

			if res.SkippedReason == "" {
				t.Fatalf("SkippedReason empty, want the rig reported as skipped")
			}
			if len(res.Deleted) != 0 || len(res.Unmerged) != 0 || len(res.Held) != 0 {
				t.Errorf("classified branches in a skipped rig: %+v", res)
			}
			if len(tc.repo.deleted) != 0 {
				t.Errorf("skipped rig deleted %v, want nothing", tc.repo.deleted)
			}
		})
	}
}

// TestPruneLocalPolecatBranchesReportsFailedBranchAndSweepsTheRest pins the
// per-branch failure path: one branch the sweep cannot resolve or delete is
// reported and the rest of the rig still runs.
func TestPruneLocalPolecatBranchesReportsFailedBranchAndSweepsTheRest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		repo *fakeLocalPruneRepo
		want string
	}{
		{
			name: "ancestry unreadable",
			repo: &fakeLocalPruneRepo{
				branches:    []string{"polecat/a/gt-1+1", "polecat/b/gt-2+2"},
				merged:      []string{"polecat/b/gt-2+2"},
				ancestryErr: map[string]error{"polecat/a/gt-1+1": errors.New("merge-base failed")},
			},
			want: "polecat/a/gt-1+1",
		},
		{
			name: "delete refused",
			repo: &fakeLocalPruneRepo{
				branches:  []string{"polecat/a/gt-1+1", "polecat/b/gt-2+2"},
				merged:    []string{"polecat/a/gt-1+1", "polecat/b/gt-2+2"},
				deleteErr: map[string]error{"polecat/a/gt-1+1": errors.New("branch is not fully merged")},
			},
			want: "polecat/a/gt-1+1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := pruneLocalPolecatBranches(tc.repo, true)

			if len(res.Failures) != 1 || !strings.Contains(res.Failures[0], tc.want) {
				t.Fatalf("failures %v, want one naming %s", res.Failures, tc.want)
			}
			if !slices.Equal(res.Deleted, []string{"polecat/b/gt-2+2"}) {
				t.Errorf("deleted %v, want the other merged branch", res.Deleted)
			}
			if res.SkippedReason != "" {
				t.Errorf("SkippedReason %q, want the rig still swept", res.SkippedReason)
			}
		})
	}
}

// TestPruneLocalPolecatBranchBacklogReportsEveryRigAndItsTotals pins the
// cross-rig driver: a rig with no repo is reported and skipped without
// stopping the rigs after it, and the totals sum what the rigs reported.
func TestPruneLocalPolecatBranchBacklogReportsEveryRigAndItsTotals(t *testing.T) {
	t.Parallel()
	repos := map[string]*fakeLocalPruneRepo{
		"/town/alpha/.repo.git": {
			branches: []string{"polecat/a/gt-1+1", "polecat/b/gt-2+2"},
			merged:   []string{"polecat/a/gt-1+1"},
			held:     []string{"polecat/b/gt-2+2"},
		},
		"/town/gamma/mayor/rig": {
			branches: []string{"polecat/c/gt-3+3"},
			merged:   []string{"polecat/c/gt-3+3"},
		},
	}
	targets := []localPruneTarget{
		{Name: "alpha", RepoDir: "/town/alpha/.repo.git"},
		{Name: "beta"},
		{Name: "gamma", RepoDir: "/town/gamma/mayor/rig"},
	}

	var out bytes.Buffer
	totals := pruneLocalPolecatBranchBacklog(&out, targets, func(dir string) localPruneRepo {
		repo, ok := repos[dir]
		if !ok {
			t.Errorf("sweep opened unknown repo %s", dir)
			return &fakeLocalPruneRepo{}
		}
		return repo
	}, true)

	want := localPruneTotals{Deleted: 2, Held: 1, Unmerged: 0, Failed: 0, Skipped: 1}
	if totals != want {
		t.Errorf("totals %+v, want %+v", totals, want)
	}
	for _, want := range []string{"alpha", "beta", "gamma", "skipped: no git repo (neither .repo.git nor mayor/rig)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report %q lacks %q", out.String(), want)
		}
	}
	if !slices.Contains(repos["/town/gamma/mayor/rig"].deleted, "polecat/c/gt-3+3") {
		t.Errorf("rig after the skipped one was not swept: deleted %v", repos["/town/gamma/mayor/rig"].deleted)
	}
}

// TestReportLocalPolecatPruneNamesKeptAndDeletedBranches pins the report an
// operator reviews before trusting a delete: the dry run says so, and every
// kept branch is named with the reason it was kept.
func TestReportLocalPolecatPruneNamesKeptAndDeletedBranches(t *testing.T) {
	t.Parallel()
	res := localPruneRigResult{
		Deleted:  []string{"polecat/a/gt-1+1"},
		Held:     []string{"polecat/b/gt-2+2"},
		Unmerged: []string{"polecat/c/gt-3+3"},
		Failures: []string{"polecat/d/gt-4+4: merge-base failed"},
	}
	for _, tc := range []struct {
		name    string
		apply   bool
		want    []string
		notWant []string
	}{
		{
			name:    "dry run",
			want:    []string{"would delete: 1", "kept, checked out: 1", "kept, unmerged: 1", "polecat/a/gt-1+1", "polecat/b/gt-2+2", "polecat/c/gt-3+3", "errors: 1"},
			notWant: []string{"deleted: 1"},
		},
		{
			name:    "applied",
			apply:   true,
			want:    []string{"deleted: 1"},
			notWant: []string{"would delete"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			reportLocalPolecatPrune(&out, "alpha", res, tc.apply)
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("report %q lacks %q", out.String(), w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out.String(), w) {
					t.Errorf("report %q says %q", out.String(), w)
				}
			}
		})
	}
}

// TestReportLocalPolecatPruneSkippedRig pins that a rig the sweep could not
// read reports why, rather than an all-zero result that reads as "nothing to
// do here".
func TestReportLocalPolecatPruneSkippedRig(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	reportLocalPolecatPrune(&out, "beta", localPruneRigResult{SkippedReason: "no git repo"}, true)

	if !strings.Contains(out.String(), "beta") || !strings.Contains(out.String(), "skipped: no git repo") {
		t.Fatalf("report %q, want beta reported as skipped with its reason", out.String())
	}
	if strings.Contains(out.String(), "deleted") {
		t.Errorf("report %q counts a delete for a skipped rig", out.String())
	}
}

// TestReportLocalPolecatPruneTotals pins the run summary's counts and verb.
func TestReportLocalPolecatPruneTotals(t *testing.T) {
	t.Parallel()
	totals := localPruneTotals{Deleted: 12, Held: 4, Unmerged: 3, Failed: 1, Skipped: 2}

	var dry bytes.Buffer
	reportLocalPolecatPruneTotals(&dry, totals, false)
	if !strings.Contains(dry.String(), "would delete 12") || !strings.Contains(dry.String(), "kept 7 (4 checked out, 3 unmerged)") {
		t.Errorf("dry total %q", dry.String())
	}
	if !strings.Contains(dry.String(), "1 error(s), 2 rig(s) skipped") {
		t.Errorf("dry total %q lacks the error and skip counts", dry.String())
	}

	var applied bytes.Buffer
	reportLocalPolecatPruneTotals(&applied, totals, true)
	if !strings.Contains(applied.String(), "deleted 12") || strings.Contains(applied.String(), "would delete") {
		t.Errorf("applied total %q", applied.String())
	}
}

// TestLocalPruneRunErrorFailsOnlyOnUnsweptBranches pins the exit status: a
// branch the sweep could not judge or delete fails the run, while a rig it
// reported and skipped does not — an unreadable rig is the designed outcome,
// not a failed sweep.
func TestLocalPruneRunErrorFailsOnlyOnUnsweptBranches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		totals localPruneTotals
		want   bool
	}{
		{name: "clean run", totals: localPruneTotals{Deleted: 3, Held: 1, Unmerged: 2}},
		{name: "skipped rigs only", totals: localPruneTotals{Deleted: 3, Skipped: 2}},
		{name: "per-branch failure", totals: localPruneTotals{Deleted: 3, Failed: 1}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := localPruneRunError(tc.totals); (err != nil) != tc.want {
				t.Errorf("localPruneRunError(%+v) = %v, want error=%v", tc.totals, err, tc.want)
			}
		})
	}
}

// TestRigPruneRepoDirPrefersTheBareRepoAndFallsBackToTheMayorClone pins the
// repo choice the single-rig prune already makes, on directories only — no
// git runs here.
func TestRigPruneRepoDirPrefersTheBareRepoAndFallsBackToTheMayorClone(t *testing.T) {
	t.Parallel()
	rigRoot := t.TempDir()
	bare := filepath.Join(rigRoot, ".repo.git")
	if err := os.MkdirAll(bare, 0755); err != nil {
		t.Fatalf("mkdir bare repo: %v", err)
	}
	if got := rigPruneRepoDir(rigRoot); got != bare {
		t.Errorf("rigPruneRepoDir = %q, want the bare repo %q", got, bare)
	}

	if err := os.RemoveAll(bare); err != nil {
		t.Fatalf("remove bare repo: %v", err)
	}
	legacy := filepath.Join(rigRoot, "mayor", "rig")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatalf("mkdir mayor clone: %v", err)
	}
	if got := rigPruneRepoDir(rigRoot); got != legacy {
		t.Errorf("rigPruneRepoDir = %q, want the mayor clone %q", got, legacy)
	}

	if err := os.RemoveAll(legacy); err != nil {
		t.Fatalf("remove mayor clone: %v", err)
	}
	if got := rigPruneRepoDir(rigRoot); got != "" {
		t.Errorf("rigPruneRepoDir = %q, want empty for a rig with no repo", got)
	}
}
