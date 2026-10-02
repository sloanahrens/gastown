package done

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/checkpoint"
)

// With real work under the checkpoints, the fold is an amend: it keeps that
// commit's message as the tip's.
func TestAutoSaveTipFixCommand_FoldsUnderRealWork(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	got := autoSaveTipFixCommand(checkpoint.AutoSaveTip{Subject: checkpoint.WIPCommitPrefix, AutoSave: true, Trailing: 2, Ahead: 2})
	if strings.Contains(got, "--amend") {
		t.Errorf("expected no amend when every commit is machine-generated, got %q", got)
	}
	if !strings.HasPrefix(got, "git reset --soft HEAD~2 && git commit -m") {
		t.Errorf("expected a reset-to-base plus a real message, got %q", got)
	}
}

// Amending a merge commit would bury the branch's own message under the
// merge's subject, so the merge case gets the explicit-message form instead.
func TestAutoSaveTipFixCommand_MergeBeneathTheRun(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestAutoSaveTipRefusalError_NamesTheTipAndTheFix(t *testing.T) {
	t.Parallel()
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
