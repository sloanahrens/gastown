package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/checkpoint"
)

// With real work under the checkpoints, the fold is an amend: it keeps that
// commit's message as the tip's.
func TestAutoSaveTipFixCommand_FoldsUnderRealWork(t *testing.T) {
	got := autoSaveTipFixCommand(checkpoint.AutoSaveTip{Subject: checkpoint.WIPCommitPrefix, AutoSave: true, Trailing: 2, Ahead: 3})
	want := "git reset --soft HEAD~2 && git commit --amend --no-edit"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// When the whole branch is machine-generated there is no real commit to amend,
// so the polecat must supply a message — amending the base commit would
// rewrite work the branch does not own.
func TestAutoSaveTipFixCommand_WholeBranchGenerated(t *testing.T) {
	got := autoSaveTipFixCommand(checkpoint.AutoSaveTip{Subject: checkpoint.WIPCommitPrefix, AutoSave: true, Trailing: 2, Ahead: 2})
	if strings.Contains(got, "--amend") {
		t.Errorf("expected no amend when every commit is machine-generated, got %q", got)
	}
	if !strings.HasPrefix(got, "git reset --soft HEAD~2 && git commit -m") {
		t.Errorf("expected a reset-to-base plus a real message, got %q", got)
	}
}

// The gt-iki6 truth table: a machine-generated tip is only submittable when
// the squash step can rewrite it, which it cannot once origin has the branch.
func TestAutoSaveTipGate(t *testing.T) {
	generated := checkpoint.AutoSaveTip{Subject: checkpoint.WIPCommitPrefix, AutoSave: true, Trailing: 1, Ahead: 2}
	real := checkpoint.AutoSaveTip{Subject: "fix: finish the feature", Trailing: 0, Ahead: 2}

	tests := []struct {
		name         string
		tip          checkpoint.AutoSaveTip
		pushedReason string
		wantRefusal  bool
	}{
		{"generated tip on an unpushed branch is squashed, not refused", generated, "", false},
		{"generated tip on a pushed branch is refused", generated, "prior push checkpoint exists", true},
		{"real tip on a pushed branch submits", real, "prior push checkpoint exists", false},
		{"real tip on an unpushed branch submits", real, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := autoSaveTipGate(tc.tip, "polecat/diamond/gt-zd7b+mucl8bqw", tc.pushedReason)
			if tc.wantRefusal && err == nil {
				t.Fatal("expected a refusal, got nil")
			}
			if !tc.wantRefusal && err != nil {
				t.Fatalf("expected no refusal, got %v", err)
			}
		})
	}
}

// The gt-iki6 loop end to end: a pushed branch whose tip is a checkpoint_dog
// commit is refused, the exact command in the refusal clears it, and the branch
// is then submittable as a single real commit.
func TestAutoSaveTipGate_RefusalCommandClearsTheTip(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "work")
	testRunGit(t, dir, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	writeRepoFile(t, repo, "README.md", "# seed\n")
	testRunGit(t, repo, "add", "-A")
	testRunGit(t, repo, "commit", "-m", "seed")

	const branch = "polecat/diamond/gt-zd7b+mucl8bqw"
	testRunGit(t, repo, "checkout", "-b", branch)
	writeRepoFile(t, repo, "real.go", "package real\n")
	testRunGit(t, repo, "add", "-A")
	testRunGit(t, repo, "commit", "-m", "feat: real work (gt-zd7b)")
	writeRepoFile(t, repo, "wip.go", "package wip\n")
	testRunGit(t, repo, "add", "-A")
	testRunGit(t, repo, "commit", "-m", checkpoint.WIPCommitPrefix)

	tip, err := checkpoint.InspectAutoSaveTip(repo, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	refusal := autoSaveTipGate(tip, branch, "prior push checkpoint exists")
	if refusal == nil {
		t.Fatal("expected a WIP tip on an already-pushed branch to be refused")
	}

	for _, part := range strings.Split(autoSaveTipFixCommand(tip), " && ") {
		testRunGit(t, repo, strings.Fields(strings.TrimPrefix(part, "git "))...)
	}

	after, err := checkpoint.InspectAutoSaveTip(repo, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if after.AutoSave {
		t.Errorf("expected the tip to be real work after the fix, got %q", after.Subject)
	}
	if after.Ahead != 1 {
		t.Errorf("expected the branch to collapse to 1 commit, got %d", after.Ahead)
	}
	if after.Subject != "feat: real work (gt-zd7b)" {
		t.Errorf("expected the real commit's message to survive as the subject, got %q", after.Subject)
	}
	if err := autoSaveTipGate(after, branch, "prior push checkpoint exists"); err != nil {
		t.Errorf("expected the folded branch to submit, got %v", err)
	}
}

// Amending a merge commit would bury the branch's own message under the
// merge's subject, so the merge case gets the explicit-message form instead.
func TestAutoSaveTipFixCommand_MergeBeneathTheRun(t *testing.T) {
	got := autoSaveTipFixCommand(checkpoint.AutoSaveTip{
		Subject: checkpoint.WIPCommitPrefix, AutoSave: true, Trailing: 1, Ahead: 2, BeneathIsMerge: true,
	})
	if strings.Contains(got, "--amend") {
		t.Errorf("expected no amend when the commit beneath is a merge, got %q", got)
	}
	if !strings.HasPrefix(got, "git reset --soft HEAD~1 && git commit -m") {
		t.Errorf("expected a reset-to-the-merge plus a real message, got %q", got)
	}
}

// A squash that died after its soft reset leaves the branch with no commits;
// the refusal has to say so rather than describe a tip that no longer exists.
func TestAutoSaveSquashResetError_NamesTheResetState(t *testing.T) {
	err := autoSaveSquashResetError("polecat/emerald/gt-iki6+mucl8bqw", "origin/main", errors.New("index.lock: File exists"))
	if err == nil {
		t.Fatal("expected a refusal")
	}

	msg := err.Error()
	for _, want := range []string{
		"polecat/emerald/gt-iki6+mucl8bqw",
		"index.lock",
		"staged on top of origin/main",
		`git commit -m`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message is missing %q:\n%s", want, msg)
		}
	}
}

func TestAutoSaveTipUninspectableError_NamesTheBranchAndTheReason(t *testing.T) {
	err := autoSaveTipUninspectableError("polecat/emerald/gt-iki6+mucl8bqw", "prior push checkpoint exists", errors.New("exit status 128"))
	if err == nil {
		t.Fatal("expected a refusal")
	}

	msg := err.Error()
	for _, want := range []string{
		"polecat/emerald/gt-iki6+mucl8bqw",
		"exit status 128",
		"prior push checkpoint exists",
		"git log --oneline",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message is missing %q:\n%s", want, msg)
		}
	}
}

func TestAutoSaveTipRefusalError_NamesTheTipAndTheFix(t *testing.T) {
	tip := checkpoint.AutoSaveTip{Subject: checkpoint.WIPCommitPrefix, AutoSave: true, Trailing: 1, Ahead: 2}
	err := autoSaveTipRefusalError(tip, "polecat/diamond/gt-zd7b+mucl8bqw", "origin already has this branch (origin/x already exists on origin from an earlier dispatch), so squashing it here would need a force-push.")
	if err == nil {
		t.Fatal("expected a refusal")
	}

	msg := err.Error()
	for _, want := range []string{
		"refusing to submit branch polecat/diamond/gt-zd7b+mucl8bqw",
		checkpoint.WIPCommitPrefix,
		"force-push",
		"git reset --soft HEAD~1 && git commit --amend --no-edit",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message is missing %q:\n%s", want, msg)
		}
	}
}
