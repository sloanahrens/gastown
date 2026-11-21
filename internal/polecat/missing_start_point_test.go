package polecat

import (
	"errors"
	"strings"
	"testing"
)

// refCheckFails is a repository whose ref check cannot answer.
type refCheckFails struct{ gitRepo }

func (refCheckFails) RefExists(string) (bool, error) { return false, errors.New("refs unreadable") }

// A start point the worktree add cannot resolve reaches the user as the
// default_branch message, and a ref check that itself fails is named beside the
// add's error: the ref is checked only after the add fails (gt-dwpx1j).
func TestAllocateAndAddMissingStartPoint(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		checkErr bool
		want     []string
	}{
		{"a missing start point names the default branch", false, []string{"configured default_branch not found as origin/nope in bare repo"}},
		{"a failing ref check is named beside the add error", true, []string{"creating worktree from origin/nope", "refs unreadable"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, _, _ := canonicalRig(t)
			if tt.checkErr {
				open := mgr.gits.openDir
				mgr.gits.openDir = func(gitDir, workDir string) gitRepo { return refCheckFails{open(gitDir, workDir)} }
			}
			_, _, err := mgr.AllocateAndAdd(AddOptions{BaseBranch: "origin/nope"})
			for _, w := range tt.want {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Errorf("AllocateAndAdd on a missing start point = %v, want it to hold %q", err, w)
				}
			}
		})
	}
}
