package polecat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initLiveGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test User"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// initLiveGitRepoWithRemote is initLiveGitRepo plus a bare origin carrying the
// main branch, so the index-skew comparison refs a real seat has (origin/main
// and the local main) both exist.
func initLiveGitRepoWithRemote(t *testing.T) string {
	t.Helper()
	dir := initLiveGitRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	for _, args := range [][]string{
		{"init", "--bare", remote},
		{"remote", "add", "origin", remote},
		{"push", "-u", "origin", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// runLiveGit drives git in a probe test worktree, failing the test on error.
func runLiveGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestProbeLiveGitState measures the three facts the reuse verdict re-derives
// from, and fails closed rather than reporting a clean worktree when it cannot
// measure at all.
func TestProbeLiveGitState(t *testing.T) {
	t.Run("clean worktree is measured live and clean", func(t *testing.T) {
		got := ProbeLiveGitState(initLiveGitRepo(t))
		if got.Source != GitStateSourceLive {
			t.Fatalf("Source = %q, want %q (reason %q)", got.Source, GitStateSourceLive, got.FailedReason)
		}
		if got.Branch != "main" {
			t.Fatalf("Branch = %q, want main", got.Branch)
		}
		if got.Dirty || got.StashCount != 0 || got.UnpushedCommits != 0 {
			t.Fatalf("clean worktree measured as dirty=%v stash=%d unpushed=%d", got.Dirty, got.StashCount, got.UnpushedCommits)
		}
	})

	t.Run("stashed worktree reports the stash count", func(t *testing.T) {
		dir := initLiveGitRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Changed\n"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		for _, args := range [][]string{{"stash", "push", "-m", "wip"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		got := ProbeLiveGitState(dir)
		if got.Source != GitStateSourceLive {
			t.Fatalf("Source = %q, want %q", got.Source, GitStateSourceLive)
		}
		if got.StashCount != 1 {
			t.Fatalf("StashCount = %d, want 1", got.StashCount)
		}
	})

	t.Run("missing worktree fails closed to unknown", func(t *testing.T) {
		got := ProbeLiveGitState(filepath.Join(t.TempDir(), "gone"))
		if got.Source != GitStateSourceUnknown {
			t.Fatalf("Source = %q, want %q", got.Source, GitStateSourceUnknown)
		}
		if !strings.Contains(got.FailedReason, "git_state=unknown") {
			t.Fatalf("FailedReason = %q, want a git_state=unknown explanation", got.FailedReason)
		}
		if got.Branch != "" || got.Dirty || got.StashCount != 0 || got.UnpushedCommits != 0 {
			t.Fatalf("failed probe reported facts: %+v", got)
		}
	})

	t.Run("a directory inside a repo is not measured as its own worktree", func(t *testing.T) {
		// A leftover polecat directory (an incomplete nuke, an obsolete layout)
		// sits INSIDE the rig's repository. Git resolves upward, so probing it
		// naively reports the rig root's branch and dirt as the polecat's — a
		// confident "live" answer about somebody else's tree. It must be
		// unmeasurable instead.
		rigRoot := initLiveGitRepo(t)
		if err := os.WriteFile(filepath.Join(rigRoot, "rig-dirt.txt"), []byte("rig root churn\n"), 0644); err != nil {
			t.Fatalf("write rig dirt: %v", err)
		}
		nested := filepath.Join(rigRoot, "polecats", "peridot", "gastown")
		if err := os.MkdirAll(nested, 0755); err != nil {
			t.Fatalf("mkdir nested: %v", err)
		}

		if IsWorktreeRoot(nested) {
			t.Fatalf("IsWorktreeRoot(%q) = true, want false (it is a plain directory inside %q)", nested, rigRoot)
		}
		got := ProbeLiveGitState(nested)
		if got.Source != GitStateSourceUnknown {
			t.Fatalf("Source = %q, want %q (probe %+v)", got.Source, GitStateSourceUnknown, got)
		}
		if got.Branch != "" {
			t.Fatalf("Branch = %q, want empty — the enclosing repo's branch must not be reported as this polecat's", got.Branch)
		}
		if got.Dirty {
			t.Fatalf("Dirty = true — the enclosing repo's dirt must not be reported as this polecat's")
		}

		// The enclosing repo is itself a legitimate worktree root, so the check
		// discriminates layout, not mere repo membership.
		if !IsWorktreeRoot(rigRoot) {
			t.Fatalf("IsWorktreeRoot(%q) = false, want true", rigRoot)
		}
	})

	// gt-ycvx: the seat verdict this probe feeds is a cleanliness check, so a
	// staged revert of a fix this checkout carries must read dirty — not as
	// index skew, which would advertise the seat as reusable with the revert
	// still in its index.
	t.Run("staged revert of a fix this checkout carries reads dirty", func(t *testing.T) {
		dir := initLiveGitRepoWithRemote(t)
		runLiveGit(t, dir, "checkout", "-b", "polecat/jasper/om-x")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test fixed\n"), 0644); err != nil {
			t.Fatalf("write fix: %v", err)
		}
		runLiveGit(t, dir, "commit", "-am", "fix: confine the reviewer to a read-only allowlist")
		runLiveGit(t, dir, "push", "-u", "origin", "polecat/jasper/om-x")
		runLiveGit(t, dir, "checkout", "origin/main", "--", "README.md")

		got := ProbeLiveGitState(dir)
		if got.Source != GitStateSourceLive {
			t.Fatalf("Source = %q, want %q (reason %q)", got.Source, GitStateSourceLive, got.FailedReason)
		}
		if !got.Dirty {
			t.Fatalf("Dirty = false for a seat holding a staged security-fix revert (probe %+v)", got)
		}
	})
}

// TestDecideWorkstateRedrivesGitFromLiveProbe is the claude-41j.1 D9 table.
// The verdict re-derives from the live probe; the recorded cleanup_status is a
// hint that can only ever make the verdict stricter, never cleaner.
func TestDecideWorkstateRedrivesGitFromLiveProbe(t *testing.T) {
	tests := []struct {
		name             string
		in               WorkstateInput
		wantVerdict      string
		wantReason       string
		wantGitStateSrc  string
		wantGitReasonHas string
	}{
		{
			// The 2026-09-10 01:14 incident: has_stash reported ten minutes
			// after the stash was dropped, with 0 stashes and a clean tree.
			name:            "stale recorded has_stash with a clean live worktree is reusable",
			in:              WorkstateInput{State: StateDone, CleanupStatus: CleanupStash, GitStateSource: GitStateSourceLive, Branch: "polecat/stale"},
			wantVerdict:     WorkstateVerdictSafeToNuke,
			wantReason:      "reusable",
			wantGitStateSrc: GitStateSourceLive,
		},
		{
			name:            "stale recorded has_unpushed with a clean live worktree is reusable",
			in:              WorkstateInput{State: StateIdle, CleanupStatus: CleanupUnpushed, GitStateSource: GitStateSourceLive},
			wantVerdict:     WorkstateVerdictSafeToNuke,
			wantReason:      "reusable",
			wantGitStateSrc: GitStateSourceLive,
		},
		{
			name:            "recorded clean cannot rescue a dirty live worktree",
			in:              WorkstateInput{State: StateDone, CleanupStatus: CleanupClean, GitStateSource: GitStateSourceLive, GitDirty: true, GitDirtyReason: "git_state=has_uncommitted uncommitted_files=2"},
			wantVerdict:     WorkstateVerdictNeedsRecovery,
			wantReason:      "git-dirty",
			wantGitStateSrc: GitStateSourceLive,
		},
		{
			name:            "recorded clean cannot rescue a stashed live worktree",
			in:              WorkstateInput{State: StateDone, CleanupStatus: CleanupClean, GitStateSource: GitStateSourceLive, StashCount: 1},
			wantVerdict:     WorkstateVerdictNeedsRecovery,
			wantReason:      "git-stash",
			wantGitStateSrc: GitStateSourceLive,
		},
		{
			name:             "failed live check fails closed with git_state=unknown",
			in:               WorkstateInput{State: StateDone, CleanupStatus: CleanupClean, GitStateSource: GitStateSourceUnknown, GitCheckFailed: true, GitCheckFailedReason: "git_state=unknown path=/gone: reading branch failed: not a repository"},
			wantVerdict:      WorkstateVerdictNeedsRecovery,
			wantReason:       "git-check-failed",
			wantGitStateSrc:  GitStateSourceUnknown,
			wantGitReasonHas: "not a repository",
		},
		{
			name:            "no live probe leaves the recorded hint authoritative",
			in:              WorkstateInput{State: StateDone, CleanupStatus: CleanupStash},
			wantVerdict:     WorkstateVerdictNeedsRecovery,
			wantReason:      "cleanup-has_stash",
			wantGitStateSrc: GitStateSourceRecorded,
		},
		{
			name:            "failed live check also keeps the recorded hint blocking",
			in:              WorkstateInput{State: StateDone, CleanupStatus: CleanupStash, GitStateSource: GitStateSourceUnknown, GitCheckFailed: true},
			wantVerdict:     WorkstateVerdictNeedsRecovery,
			wantReason:      "cleanup-has_stash",
			wantGitStateSrc: GitStateSourceUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideWorkstate(tt.in)
			if got.Verdict != tt.wantVerdict || got.Reason != tt.wantReason {
				t.Fatalf("DecideWorkstate() = %s/%s, want %s/%s", got.Verdict, got.Reason, tt.wantVerdict, tt.wantReason)
			}
			if got.Reusable != (tt.wantVerdict == WorkstateVerdictSafeToNuke) {
				t.Fatalf("Reusable = %v for verdict %s", got.Reusable, got.Verdict)
			}
			if got.GitStateSource != tt.wantGitStateSrc {
				t.Fatalf("GitStateSource = %q, want %q", got.GitStateSource, tt.wantGitStateSrc)
			}
			if got.CleanupStatusSource != CleanupStatusSourceRecorded {
				t.Fatalf("CleanupStatusSource = %q, want %q", got.CleanupStatusSource, CleanupStatusSourceRecorded)
			}
			if tt.wantGitReasonHas != "" && !strings.Contains(got.GitStateReason, tt.wantGitReasonHas) {
				t.Fatalf("GitStateReason = %q, want it to mention %q", got.GitStateReason, tt.wantGitReasonHas)
			}
			if tt.wantGitStateSrc == GitStateSourceUnknown && got.GitStateReason == "" {
				t.Fatalf("GitStateReason empty for a failed live check: %+v", got)
			}
		})
	}
}

func TestRecordedCleanupBlocks(t *testing.T) {
	tests := []struct {
		name   string
		status CleanupStatus
		source string
		want   bool
	}{
		{name: "recorded clean never blocks", status: CleanupClean, source: GitStateSourceRecorded, want: false},
		{name: "live clean supersedes recorded has_uncommitted", status: CleanupUncommitted, source: GitStateSourceLive, want: false},
		{name: "live clean supersedes recorded has_stash", status: CleanupStash, source: GitStateSourceLive, want: false},
		{name: "live clean supersedes recorded has_unpushed", status: CleanupUnpushed, source: GitStateSourceLive, want: false},
		{name: "no probe keeps recorded has_stash blocking", status: CleanupStash, source: GitStateSourceRecorded, want: true},
		{name: "failed probe keeps recorded has_stash blocking", status: CleanupStash, source: GitStateSourceUnknown, want: true},
		{name: "unset source keeps recorded has_uncommitted blocking", status: CleanupUncommitted, source: "", want: true},
		// gt-14a/gt-ui2x: unlike the git-derived statuses above, a live probe
		// alone must NOT clear missing/unknown — RecordedCleanupBlocks cannot
		// tell whether the agent bead behind the missing self-report was ever
		// read (hook_bead/push_failed/mr_failed/active_mr may be completely
		// unverified). That narrower, agent-bead-read-gated escape lives in
		// ResolveIgnoreCleanupStatus instead (see
		// TestNewWorkstateInputMissingCleanupStatusClearsOnLiveCleanProbe and
		// TestNewWorkstateInputMissingCleanupStatusStillBlocksWithoutAgentBeadRead).
		{name: "live probe alone does not clear missing status", status: "", source: GitStateSourceLive, want: true},
		{name: "live probe alone does not clear unknown status", status: CleanupUnknown, source: GitStateSourceLive, want: true},
		{name: "no probe keeps missing status blocking", status: "", source: GitStateSourceRecorded, want: true},
		{name: "no probe keeps unknown status blocking", status: CleanupUnknown, source: GitStateSourceRecorded, want: true},
		{name: "failed probe keeps missing status blocking", status: "", source: GitStateSourceUnknown, want: true},
		{name: "failed probe keeps unknown status blocking", status: CleanupUnknown, source: GitStateSourceUnknown, want: true},
		{name: "unset source keeps missing status blocking", status: "", source: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RecordedCleanupBlocks(tt.status, tt.source); got != tt.want {
				t.Fatalf("RecordedCleanupBlocks(%q, %q) = %v, want %v", tt.status, tt.source, got, tt.want)
			}
		})
	}
}
