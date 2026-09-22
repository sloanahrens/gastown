package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

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
	t.Parallel()
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

// TestPolecatIntegrationEnabledReadsRigRootMergeQueue reproduces the gt-xwt9
// finding: both SpawnPolecatForSling call sites read integration_branch_
// polecat_enabled from rig-local settings/config.json only via
// config.LoadRigSettings, so a rig-root-only false (gt-me9t's floor) was
// silently ignored and integration sourcing stayed on. Routing through
// rig.ResolveMergeQueueConfig (as resolveSetupCommand and
// buildRefineryPatrolVars already do) makes the rig-root value visible with
// no rig-local settings/config.json present at all.
func TestPolecatIntegrationEnabledReadsRigRootMergeQueue(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "merge_queue": {"integration_branch_polecat_enabled": false}
}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := polecatIntegrationEnabled(townRoot, rigName); got != false {
		t.Errorf("polecatIntegrationEnabled() = %v, want false (rig-root merge_queue floor invisible to spawn)", got)
	}
}

// TestPolecatIntegrationEnabledDefaultsTrue guards the nil-safe default: no
// config at any tier must not disable integration sourcing.
func TestPolecatIntegrationEnabledDefaultsTrue(t *testing.T) {
	townRoot := t.TempDir()
	if got := polecatIntegrationEnabled(townRoot, "norig"); got != true {
		t.Errorf("polecatIntegrationEnabled() = %v, want true (no config at any tier defaults enabled)", got)
	}
}

func TestEffectivePolecatDirCap(t *testing.T) {
	t.Parallel()
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
