package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// rigGitBaseLayouts are the rig layouts the git base helpers must refuse: a
// rig is only a git base when its shared bare .repo.git is a directory. A
// mayor/rig clone, on its own, is not one.
var rigGitBaseLayouts = []struct {
	name  string
	setup func(t *testing.T, rigPath string)
}{
	{
		name: "mayor clone and no bare repo",
		setup: func(t *testing.T, rigPath string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	},
	{
		name:  "no git base at all",
		setup: func(*testing.T, string) {},
	},
	{
		name: "bare path is a file, not a directory",
		setup: func(t *testing.T, rigPath string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(rigPath, ".repo.git"), []byte("gitdir: x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	},
}

// requireNoRepoGitError checks a refused rig reports the one error the cmd
// package uses for it, naming the rig path.
func requireNoRepoGitError(t *testing.T, rigPath string, g *git.Git, err error) {
	t.Helper()
	want := "no .repo.git under " + rigPath
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if g != nil {
		t.Errorf("git client = %v on refusal, want nil", g)
	}
}

func TestGetRigGitRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range rigGitBaseLayouts {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigPath := t.TempDir()
			tc.setup(t, rigPath)
			g, err := getRigGit(rigPath)
			requireNoRepoGitError(t, rigPath, g, err)
		})
	}
}

func TestGetRigGitOpensTheBareRepo(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := getRigGit(rigPath)
	if err != nil {
		t.Fatalf("getRigGit = %v, want the bare repo", err)
	}
	if g == nil {
		t.Fatal("getRigGit returned a nil client for a rig with a bare repo")
	}
}

func TestGetRepoGitForRigRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range rigGitBaseLayouts {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigPath := t.TempDir()
			tc.setup(t, rigPath)
			g, err := getRepoGitForRig(rigPath)
			requireNoRepoGitError(t, rigPath, g, err)
		})
	}
}

func TestStartCleanupRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range rigGitBaseLayouts {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigPath := t.TempDir()
			tc.setup(t, rigPath)
			err := deleteRigPolecatBranch(rigPath, "polecat/max")
			want := "no .repo.git under " + rigPath
			if err == nil || err.Error() != want {
				t.Fatalf("deleteRigPolecatBranch = %v, want %q", err, want)
			}
		})
	}
}

// TestSlingBatchSandboxRefusesWithoutBareRepo pins the sling rollback: a rig
// with no .repo.git has no branch the sandbox can delete, so the sandbox
// returns the refusal and keeps the branch rather than reaching for a
// mayor/rig clone.
func TestSlingBatchSandboxRefusesWithoutBareRepo(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := rigPolecatSandbox{rigPath: rigPath}.deleteBranch("polecat/max")
	want := "no .repo.git under " + rigPath
	if err == nil || err.Error() != want {
		t.Fatalf("deleteBranch = %v, want %q", err, want)
	}
}

// TestPruneLocalRefusesARigWithoutBareRepo pins the prune's refusal: a rig
// with no .repo.git is an error that names the bare path, and the prune does
// not fall back to a mayor/rig clone.
func TestPruneLocalRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := pruneRigPolecatBranches(nil, &rig.Rig{Name: "testrig", Path: rigPath})
	want := "no .repo.git under " + rigPath
	if err == nil || err.Error() != want {
		t.Fatalf("pruneRigPolecatBranches = %v, want %q", err, want)
	}
}

func TestPruneLocalRigRepoDirIsTheBareRepoOnly(t *testing.T) {
	t.Parallel()
	rigRoot := t.TempDir()
	bare := filepath.Join(rigRoot, ".repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare repo: %v", err)
	}
	if got := rigPruneRepoDir(rigRoot); got != bare {
		t.Errorf("rigPruneRepoDir = %q, want the bare repo %q", got, bare)
	}

	if err := os.RemoveAll(bare); err != nil {
		t.Fatalf("remove bare repo: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
		t.Fatalf("mkdir mayor clone: %v", err)
	}
	if got := rigPruneRepoDir(rigRoot); got != "" {
		t.Errorf("rigPruneRepoDir = %q, want empty for a rig with only a mayor clone", got)
	}
}

// TestSpawnIntegrationDetectionWarnsWithoutRepoGit pins that a rig with no
// .repo.git turns integration-branch detection off with a warning that names
// the missing bare repo, not silently.
func TestSpawnIntegrationDetectionWarnsWithoutRepoGit(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	got := detectSpawnIntegrationBranchTo(&warn, townRoot, "testrig", &rig.Rig{Name: "testrig", Path: rigPath}, "gt-abc")
	if got != "" {
		t.Errorf("detectSpawnIntegrationBranchTo = %q, want no branch for a rig without .repo.git", got)
	}
	if want := "no .repo.git under " + rigPath; !strings.Contains(warn.String(), want) {
		t.Errorf("warning = %q, want it to name %q", warn.String(), want)
	}
}

// TestStartCleanupPolecatBranchReportsUndeletedBranch pins that a polecat whose
// branch was not deleted is not reported as cleaned up.
func TestStartCleanupPolecatBranchReportsUndeletedBranch(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if deletePolecatBranchReport(&out, rigPath, "testrig", "max") {
		t.Fatal("deletePolecatBranchReport = true, want false: the branch was not deleted")
	}
	got := out.String()
	if !strings.Contains(got, "not deleted") {
		t.Errorf("output = %q, want it to report the branch not deleted", got)
	}
	if strings.Contains(got, "cleaned up") {
		t.Errorf("output = %q, claims cleaned up after the branch was not deleted", got)
	}
}
