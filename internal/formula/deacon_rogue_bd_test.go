package formula

import (
	"strings"
	"testing"
)

// TestDeaconRogueBdCheckSparesBuildOutputOfTheBdRepo pins gt-5zsc: step 18
// flagged <worktree-root>/bd in every beads worktree that had ever run `make
// build`, and neutralizing those (chmod 644) breaks the rig's own build and
// test loop. The step keeps the exemption - git-ignored build output of a
// worktree carrying cmd/bd is by-design - while a symlink, an on-PATH bd, and
// an unignored copy all stay findings.
func TestDeaconRogueBdCheckSparesBuildOutputOfTheBdRepo(t *testing.T) {
	d := deaconPatrolStep(t, "rogue-bd-check").Description

	for _, want := range []string{
		"cmd/bd",
		"check-ignore",
		"BY-DESIGN",
		"on-PATH",
		"unignored copy",
		`[ -L "$hit" ]`,
	} {
		if !strings.Contains(d, want) {
			t.Errorf("rogue-bd-check does not mention %q", want)
		}
	}

	if strings.Contains(d, "under an agent directory is a finding") {
		t.Error("rogue-bd-check still calls every executable named bd a finding, so the beads rig's own make build output is flagged again")
	}
}
