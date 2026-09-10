package cmd

import "testing"

// TestResolveSpawnBaseBranch guards gt-a8i3: resolveSpawnBaseBranch has no
// resumeBranch parameter at all, so a resume dispatch can never leak its
// resume branch into the merge-target base branch. This regression covers
// the two call sites in SpawnPolecatForSling (idle-reuse and fresh-allocate),
// both of which previously did:
//
//	if opts.ResumeBranch != "" {
//	    effectiveBranch = opts.ResumeBranch // BUG: corrupts the merge target
//	}
func TestResolveSpawnBaseBranch(t *testing.T) {
	tests := []struct {
		name          string
		baseBranch    string
		defaultBranch string
		want          string
	}{
		{"empty base falls back to rig default", "", "main", "main"},
		{"explicit base branch used as-is", "develop", "main", "develop"},
		{"origin/ prefix stripped", "origin/develop", "main", "develop"},
		{"auto-detected integration branch prefix stripped", "origin/integration/epic-1", "main", "integration/epic-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveSpawnBaseBranch(tt.baseBranch, tt.defaultBranch); got != tt.want {
				t.Errorf("resolveSpawnBaseBranch(%q, %q) = %q, want %q", tt.baseBranch, tt.defaultBranch, got, tt.want)
			}
		})
	}
}

func TestEffectivePolecatDirCap(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{"negative uses floor", -1, minPolecatDirsPerRig},
		{"zero uses floor", 0, minPolecatDirsPerRig},
		{"default below floor uses floor", 10, minPolecatDirsPerRig},
		{"one below floor uses floor", minPolecatDirsPerRig - 1, minPolecatDirsPerRig},
		{"floor remains floor", minPolecatDirsPerRig, minPolecatDirsPerRig},
		{"above floor is honored", 45, 45},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectivePolecatDirCap(tt.configured); got != tt.want {
				t.Errorf("effectivePolecatDirCap(%d) = %d, want %d", tt.configured, got, tt.want)
			}
		})
	}
}
