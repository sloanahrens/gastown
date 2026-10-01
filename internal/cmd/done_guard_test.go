package cmd

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/done"
)

// These tests cover the command layer's half of gt done: the pre-run worktree
// guard decided by the command tree, and the process exit code a coded gt done
// failure maps to. The submission path itself is tested in internal/done.

// TestDoneNeedsPolecatWorktree: the pre-run worktree guard applies to every
// gt done that is not positively crew; a crew gt done reaches runDoneCrew,
// and a polecat worktree with no polecat env hits the guard (gt-avwp2).
func TestDoneNeedsPolecatWorktree(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "gt"}
	done := &cobra.Command{Use: "done"}
	root.AddCommand(done)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	const (
		crewDir    = "/town/gastown/crew/sloan"
		polecatDir = "/town/gastown/polecats/refuge/gastown"
	)
	if doneNeedsPolecatWorktree(done, env(map[string]string{"BD_ACTOR": "gastown/crew/sloan"}), crewDir) {
		t.Error("crew gt done hit the polecat worktree guard")
	}
	if doneNeedsPolecatWorktree(done, env(map[string]string{}), crewDir) {
		t.Error("gt done in a crew worktree with no identity hit the polecat worktree guard")
	}
	if !doneNeedsPolecatWorktree(done, env(map[string]string{}), polecatDir) {
		t.Error("gt done in a polecat worktree that lost its env skipped the guard")
	}
	if !doneNeedsPolecatWorktree(done, env(map[string]string{}), "/town") {
		t.Error("gt done with no identity outside a crew worktree skipped the guard")
	}
	if !doneNeedsPolecatWorktree(done, env(map[string]string{"BD_ACTOR": "gastown/polecats/refuge", "GT_POLECAT": "refuge"}), polecatDir) {
		t.Error("polecat gt done skipped the worktree guard")
	}
	if doneNeedsPolecatWorktree(root, env(map[string]string{"GT_POLECAT": "refuge"}), polecatDir) {
		t.Error("a command other than gt done hit the worktree guard")
	}
}

func TestExecuteExitCodeForCodedError(t *testing.T) {
	t.Parallel()
	if got := exitCodeForError(&done.ExitCodeError{Code: 12, Err: errors.New("x")}); got != 12 {
		t.Errorf("exitCodeForError(coded 12) = %d", got)
	}
	if got := exitCodeForError(NewSilentExit(3)); got != 3 {
		t.Errorf("exitCodeForError(silent 3) = %d", got)
	}
	if got := exitCodeForError(errors.New("plain")); got != 1 {
		t.Errorf("exitCodeForError(plain) = %d", got)
	}
}
