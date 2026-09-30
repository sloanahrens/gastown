package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// fakeGitState is a worktree as gitStateOf reads it.
type fakeGitState struct {
	work        git.UncommittedWorkStatus
	branch      string
	preserved   git.BranchPreservationStatus
	preserveErr error
	stashes     int
	targets     []string
}

func (f *fakeGitState) CheckUncommittedWork() (*git.UncommittedWorkStatus, error) {
	w := f.work
	return &w, nil
}
func (f *fakeGitState) CurrentBranch() (string, error) { return f.branch, nil }
func (f *fakeGitState) BranchPreservationStatus(branch, remote string, targets []string) (git.BranchPreservationStatus, error) {
	f.targets = targets
	return f.preserved, f.preserveErr
}
func (f *fakeGitState) StashCountAll() (int, error) { return f.stashes, nil }

// TestGitStateVerdicts pins what makes a worktree dirty for the nuke and
// recovery gates: real uncommitted files (runtime artifacts do not count),
// the branch's own stashes (other branches' stashes are only reported),
// unpreserved commits, and a preservation check that could not run, which
// fails closed (gt-14a).
func TestGitStateVerdicts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fake  fakeGitState
		clean bool
		check func(t *testing.T, s *GitState)
	}{
		{name: "clean", fake: fakeGitState{branch: "polecat/a"}, clean: true},
		{
			name:  "runtime artifacts only",
			fake:  fakeGitState{work: git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".opencode/plugins/gastown.js", ".beads/issues.jsonl"}}},
			clean: true,
			check: func(t *testing.T, s *GitState) {
				if len(s.UncommittedFiles) != 0 {
					t.Errorf("UncommittedFiles = %v, want none", s.UncommittedFiles)
				}
			},
		},
		{
			name: "real file beside runtime artifacts",
			fake: fakeGitState{work: git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".opencode/plugins/gastown.js", "real.go"}}},
			check: func(t *testing.T, s *GitState) {
				if strings.Join(s.UncommittedFiles, ",") != "real.go" {
					t.Errorf("UncommittedFiles = %v, want [real.go]", s.UncommittedFiles)
				}
			},
		},
		{
			name: "staged-only content stays dirty (no index-skew relaxation here)",
			fake: fakeGitState{work: git.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{"README.md"}, StagedOnly: []string{"README.md"}}},
			check: func(t *testing.T, s *GitState) {
				if strings.Join(s.UncommittedFiles, ",") != "README.md" {
					t.Errorf("UncommittedFiles = %v, want [README.md]", s.UncommittedFiles)
				}
			},
		},
		{
			name: "own stash dirty, shared stashes reported",
			fake: fakeGitState{work: git.UncommittedWorkStatus{StashCount: 1}, stashes: 3},
			check: func(t *testing.T, s *GitState) {
				if s.StashCount != 1 || s.SharedStashCount != 2 {
					t.Errorf("stashes own %d shared %d, want 1 and 2", s.StashCount, s.SharedStashCount)
				}
			},
		},
		{
			name:  "only other branches' stashes",
			fake:  fakeGitState{stashes: 2},
			clean: true,
			check: func(t *testing.T, s *GitState) {
				if s.StashCount != 0 || s.SharedStashCount != 2 {
					t.Errorf("stashes own %d shared %d, want 0 and 2", s.StashCount, s.SharedStashCount)
				}
			},
		},
		{
			name: "unpreserved commits",
			fake: fakeGitState{preserved: git.BranchPreservationStatus{ComparisonBase: "upstream/main", UnpreservedPatchCount: 2}},
			check: func(t *testing.T, s *GitState) {
				if s.UnpushedCommits != 2 || s.ComparisonBase != "upstream/main" {
					t.Errorf("unpushed %d base %q, want 2 against upstream/main", s.UnpushedCommits, s.ComparisonBase)
				}
			},
		},
		{
			name: "preservation check fails closed",
			fake: fakeGitState{preserveErr: errors.New("no comparison ref")},
			check: func(t *testing.T, s *GitState) {
				if !s.PreservationCheckFailed || !strings.Contains(s.PreservationCheckFailure, "no comparison ref") {
					t.Errorf("state %+v does not record the failed check", s)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := tc.fake
			s, err := gitStateOf(&fake, []string{"release"})
			if err != nil {
				t.Fatalf("gitStateOf: %v", err)
			}
			if s.Clean != tc.clean {
				t.Errorf("Clean = %v, want %v (%+v)", s.Clean, tc.clean, s)
			}
			if strings.Join(fake.targets, ",") != "release" {
				t.Errorf("preservation judged against %v, want the given targets", fake.targets)
			}
			if tc.check != nil {
				tc.check(t, s)
			}
		})
	}
}

func setupGitStateRemoteRepo(t *testing.T) string {
	t.Helper()
	return cachedGitFixtureStrings(t, "setupGitStateRemoteRepo", func(dir string) []string {
		return []string{buildSetupGitStateRemoteRepo(t, dir)}
	})[0]
}

// buildSetupGitStateRemoteRepo makes setupGitStateRemoteRepo's repos under dir.
func buildSetupGitStateRemoteRepo(t *testing.T, dir string) string {
	t.Helper()
	remote := filepath.Join(dir, "remote.git")
	repo := filepath.Join(dir, "repo")
	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, "", "init", repo)
	runGitCmd(t, repo, "config", "user.email", "test@example.com")
	runGitCmd(t, repo, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(repo, "README.md"), "base\n")
	runGitCmd(t, repo, "add", "README.md")
	runGitCmd(t, repo, "commit", "-m", "base")
	runGitCmd(t, repo, "branch", "-M", "main")
	runGitCmd(t, repo, "remote", "add", "origin", remote)
	runGitCmd(t, repo, "push", "-u", "origin", "main")
	runGitCmd(t, repo, "switch", "-c", "integration/test")
	runGitCmd(t, repo, "push", "-u", "origin", "integration/test")
	return repo
}

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
