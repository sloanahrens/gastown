package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
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

// idlePolecatReuseFake stands in for the polecat manager's idle-reuse surface.
type idlePolecatReuseFake struct {
	idle     *polecat.Polecat
	findErr  error
	reuseErr error
	getCalls int
}

func (f *idlePolecatReuseFake) FindIdlePolecat() (*polecat.Polecat, error) {
	return f.idle, f.findErr
}

func (f *idlePolecatReuseFake) ReuseIdlePolecat(name string, opts polecat.AddOptions) (*polecat.Polecat, error) {
	if f.reuseErr != nil {
		return nil, f.reuseErr
	}
	return &polecat.Polecat{Name: name, Branch: opts.ResumeBranch}, nil
}

func (f *idlePolecatReuseFake) Get(name string) (*polecat.Polecat, error) {
	f.getCalls++
	return f.idle, nil
}

// TestReuseIdlePolecatForSling_StopsOnHeldBranch guards gt-0kk2: a refusal to
// take a branch another worktree holds must stop the sling, not fall through to
// the fresh allocation, which attaches a worktree to that same branch with
// `git worktree add --force` and so rebuilds the conflict the refusal found.
func TestReuseIdlePolecatForSling_StopsOnHeldBranch(t *testing.T) {
	branch := "polecat/quartz/gt-9ed0+mudclpwf"
	fake := &idlePolecatReuseFake{
		idle: &polecat.Polecat{Name: "alpha"},
		reuseErr: fmt.Errorf("refusing to reuse alpha: %w\nRelease that branch there",
			fmt.Errorf("%w: %s is already checked out at /town/rig/polecats/beta/rig",
				polecat.ErrBranchHeld, branch)),
	}

	info, err := reuseIdlePolecatForSling(fake, tmux.NewTmux(), &rig.Rig{Name: "rig", Path: t.TempDir()},
		t.TempDir(), "rig", SlingSpawnOptions{HookBead: "gt-next", ResumeBranch: branch}, func() {})

	if err == nil {
		t.Fatal("reuse refusal was swallowed and the sling continued to allocation; want the error")
	}
	if !errors.Is(err, polecat.ErrBranchHeld) {
		t.Errorf("sling error does not carry ErrBranchHeld, so callers cannot act on it: %v", err)
	}
	if info != nil {
		t.Errorf("sling returned %+v alongside the refusal", info)
	}
	if fake.getCalls != 0 {
		t.Errorf("sling read the polecat back %d times after a refusal; want none", fake.getCalls)
	}
}

// TestReuseIdlePolecatForSling_FallsBackOnRecoverableReuseFailure keeps the safe
// half of the fallback: a polecat that needs recovery is not a held branch, so
// the caller still allocates a fresh one.
func TestReuseIdlePolecatForSling_FallsBackOnRecoverableReuseFailure(t *testing.T) {
	fake := &idlePolecatReuseFake{
		idle:     &polecat.Polecat{Name: "alpha"},
		reuseErr: fmt.Errorf("%w: uncommitted work in worktree", polecat.ErrPolecatNeedsRecovery),
	}

	info, err := reuseIdlePolecatForSling(fake, tmux.NewTmux(), &rig.Rig{Name: "rig", Path: t.TempDir()},
		t.TempDir(), "rig", SlingSpawnOptions{HookBead: "gt-next"}, func() {})

	if err != nil {
		t.Fatalf("recoverable reuse failure aborted the sling: %v", err)
	}
	if info != nil {
		t.Errorf("expected the caller to allocate fresh, got %+v", info)
	}
}

// TestReuseIdlePolecatForSling_NoIdlePolecat covers the ordinary no-op paths:
// nothing to reuse, and a lookup that failed.
func TestReuseIdlePolecatForSling_NoIdlePolecat(t *testing.T) {
	for _, tt := range []struct {
		name string
		fake *idlePolecatReuseFake
	}{
		{"empty pool", &idlePolecatReuseFake{}},
		{"lookup failed", &idlePolecatReuseFake{findErr: errors.New("beads unavailable")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info, err := reuseIdlePolecatForSling(tt.fake, tmux.NewTmux(),
				&rig.Rig{Name: "rig", Path: t.TempDir()}, t.TempDir(), "rig",
				SlingSpawnOptions{HookBead: "gt-next"}, func() {})

			if err != nil || info != nil {
				t.Errorf("reuseIdlePolecatForSling gave (%+v, %v), want (nil, nil) so the caller allocates", info, err)
			}
		})
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
