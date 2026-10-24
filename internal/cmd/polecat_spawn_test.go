package cmd

import (
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
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
	t.Parallel()
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
	t.Parallel()
	townRoot := t.TempDir()
	if got := polecatIntegrationEnabled(townRoot, "norig"); got != true {
		t.Errorf("polecatIntegrationEnabled() = %v, want true (no config at any tier defaults enabled)", got)
	}
}

// idlePolecatReuseFake stands in for the polecat manager's idle-reuse surface.
// released records what the reuse path handed back, in order; reuseOpts records
// the options that path handed the manager instead.
type idlePolecatReuseFake struct {
	idle     *polecat.Polecat
	seats    []*polecat.Polecat // the pool's reusable seats, in order; idle alone when nil
	findErr  error
	reuseErr error
	// reuseErrFor refuses the named seats only; reuseNames records every seat
	// the path asked to reuse, in order.
	reuseErrFor map[string]error
	reuseNames  []string
	getErr      error
	getCalls    int
	released    []idleReuseRelease
	reuseOpts   []polecat.AddOptions
}

// idleReuseRelease is one release the reuse path asked for.
type idleReuseRelease struct {
	rigName     string
	polecatName string
	hookBead    string
}

func (f *idlePolecatReuseFake) IdlePolecats() (iter.Seq[*polecat.Polecat], error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	seats := f.seats
	if seats == nil && f.idle != nil {
		seats = []*polecat.Polecat{f.idle}
	}
	return slices.Values(seats), nil
}

func (f *idlePolecatReuseFake) ReuseIdlePolecat(name string, opts polecat.AddOptions) (*polecat.Polecat, error) {
	f.reuseOpts = append(f.reuseOpts, opts)
	f.reuseNames = append(f.reuseNames, name)
	if err := f.reuseErrFor[name]; err != nil {
		return nil, err
	}
	if f.reuseErr != nil {
		return nil, f.reuseErr
	}
	return &polecat.Polecat{Name: name, Branch: opts.ResumeBranch}, nil
}

func (f *idlePolecatReuseFake) Get(name string) (*polecat.Polecat, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.idle == nil {
		return &polecat.Polecat{Name: name}, nil
	}
	return f.idle, nil
}

// releaseRecordingEnv is fakeIdleReuseEnv with the release seam pointed at
// fake, so a test can see what the reuse path handed back, and a worktree check
// that fails with verifyErr.
func releaseRecordingEnv(fake *idlePolecatReuseFake, verifyErr error) idleReuseEnv {
	env := fakeIdleReuseEnv()
	env.verifyWorktree = func(string) error { return verifyErr }
	env.releaseReused = func(rigName, polecatName, hookBead string) {
		fake.released = append(fake.released, idleReuseRelease{rigName, polecatName, hookBead})
	}
	return env
}

// TestReuseIdlePolecatForSling_StopsOnHeldBranch guards gt-0kk2: a refusal to
// take a branch another worktree holds must stop the sling, not fall through to
// the fresh allocation, which attaches a worktree to that same branch with
// `git worktree add --force` and so rebuilds the conflict the refusal found.
func TestReuseIdlePolecatForSling_StopsOnHeldBranch(t *testing.T) {
	t.Parallel()
	branch := "polecat/quartz/gt-9ed0+mudclpwf"
	fake := &idlePolecatReuseFake{
		idle: &polecat.Polecat{Name: "alpha"},
		reuseErr: fmt.Errorf("refusing to reuse alpha: %w\nRelease that branch there",
			fmt.Errorf("%w: %s is already checked out at /town/rig/polecats/beta/rig",
				polecat.ErrBranchHeld, branch)),
	}

	info, err := reuseIdlePolecatForSlingWith(fake, fakeIdleReuseEnv(), "rig",
		SlingSpawnOptions{HookBead: "gt-next", ResumeBranch: branch}, func() int { return 0 })

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

// TestReuseIdlePolecatForSling_ResumeBranchHeldFallsBack is the rework side of
// the gt-0kk2 stop: a rework's seat preference is soft, so the rejected
// attempt's branch being checked out elsewhere hands the sling to the pool,
// which names a fresh branch and cannot collide with the held one. An explicit
// resume (StopsOnHeldBranch above) still stops.
func TestReuseIdlePolecatForSling_ResumeBranchHeldFallsBack(t *testing.T) {
	t.Parallel()
	fake := authorSeat(nil)
	fake.reuseErr["garnet"] = fmt.Errorf("%w: %s is already checked out at /town/rig/polecats/beta/rig",
		polecat.ErrBranchHeld, reworkBranch)

	info, err := reuseForRework(t, fake, SlingSpawnOptions{HookBead: reworkBead},
		map[string]*reworkSeat{reworkBead: {PolecatName: "garnet", Branch: reworkBranch}})

	if err != nil {
		t.Fatalf("a rework whose branch is held failed the sling instead of falling back: %v", err)
	}
	if info != nil {
		t.Errorf("expected the caller to allocate fresh, got %+v", info)
	}
	if got := fake.reuseOpts["garnet"]; !got.AllowRecovery || got.ResumeBranch != reworkBranch {
		t.Errorf("reuse options = %+v; the test must exercise the rework resume", got)
	}
}

// TestReuseIdlePolecatForSling_FallsBackOnRecoverableReuseFailure keeps the safe
// half of the fallback: a polecat that needs recovery is not a held branch, so
// the caller still allocates a fresh one.
func TestReuseIdlePolecatForSling_FallsBackOnRecoverableReuseFailure(t *testing.T) {
	t.Parallel()
	fake := &idlePolecatReuseFake{
		idle:     &polecat.Polecat{Name: "alpha"},
		reuseErr: fmt.Errorf("%w: uncommitted work in worktree", polecat.ErrPolecatNeedsRecovery),
	}

	info, err := reuseIdlePolecatForSlingWith(fake, fakeIdleReuseEnv(), "rig",
		SlingSpawnOptions{HookBead: "gt-next"}, func() int { return 0 })

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
	t.Parallel()
	for _, tt := range []struct {
		name string
		fake *idlePolecatReuseFake
	}{
		{"empty pool", &idlePolecatReuseFake{}},
		{"lookup failed", &idlePolecatReuseFake{findErr: errors.New("beads unavailable")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info, err := reuseIdlePolecatForSlingWith(tt.fake, fakeIdleReuseEnv(), "rig",
				SlingSpawnOptions{HookBead: "gt-next"}, func() int { return 0 })

			if err != nil || info != nil {
				t.Errorf("reuseIdlePolecatForSling gave (%+v, %v), want (nil, nil) so the caller allocates", info, err)
			}
		})
	}
}

// TestReuseIdlePolecatForSling_VerifyFailureReleasesSlot guards gt-pa676: the
// reuse writes the agent bead (hook_bead set, agent_state=spawning) before the
// path can read the polecat back or check its sandbox, so both failures have to
// hand the polecat back. Left hooked, the sling's own open bead reads as active
// work, the reuse gate refuses the seat and the pool skips it, so it holds a
// slot no sling can take until an operator nukes it.
func TestReuseIdlePolecatForSling_VerifyFailureReleasesSlot(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		fake      *idlePolecatReuseFake
		verifyErr error
	}{
		{
			name: "worktree verification fails",
			fake: &idlePolecatReuseFake{
				idle: &polecat.Polecat{Name: "alpha", ClonePath: "/town/rig/polecats/alpha/rig"},
			},
			verifyErr: errors.New("worktree is gone"),
		},
		{
			// Get reports only a polecat whose sandbox is gone — a
			// loadFromBeads read never fails — so this is the nuke-or-reaper
			// race window between the reuse and the read-back.
			name: "read-back fails",
			fake: &idlePolecatReuseFake{
				idle:   &polecat.Polecat{Name: "alpha"},
				getErr: polecat.ErrPolecatNotFound,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info, err := reuseIdlePolecatForSlingWith(tt.fake, releaseRecordingEnv(tt.fake, tt.verifyErr), "rig",
				SlingSpawnOptions{HookBead: "gt-next"}, func() int { return 0 })

			if err == nil {
				t.Fatal("sling went on with a reused polecat it could not verify; want the error")
			}
			if info != nil {
				t.Errorf("sling returned %+v alongside the error", info)
			}
			want := []idleReuseRelease{{rigName: "rig", polecatName: "alpha", hookBead: "gt-next"}}
			if !slices.Equal(tt.fake.released, want) {
				t.Errorf("released %+v, want exactly %+v", tt.fake.released, want)
			}
		})
	}
}

// TestReuseIdlePolecatForSling_CarriesAgentIntoAddOptions guards gt-smab3: the
// agent a sling was told to spawn the seat on has to reach the manager in the
// same AddOptions as the hook bead, because the manager writes both to the
// agent bead in one write (gt-y6cja). A sling that names no agent passes none
// through, leaving the manager to apply the role's default.
func TestReuseIdlePolecatForSling_CarriesAgentIntoAddOptions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		agent string
		want  string
	}{
		{"sling's agent reaches the manager", "deepseek-flash", "deepseek-flash"},
		{"no agent passes none through", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &idlePolecatReuseFake{
				idle: &polecat.Polecat{Name: "alpha", ClonePath: "/town/rig/polecats/alpha/rig"},
			}

			info, err := reuseIdlePolecatForSlingWith(fake, fakeIdleReuseEnv(), "rig",
				SlingSpawnOptions{HookBead: "gt-next", Agent: tt.agent}, func() int { return 0 })

			if err != nil || info == nil {
				t.Fatalf("reuseIdlePolecatForSling = (%+v, %v), want the reused polecat", info, err)
			}
			if len(fake.reuseOpts) != 1 {
				t.Fatalf("the manager was handed %d AddOptions, want 1", len(fake.reuseOpts))
			}
			got := fake.reuseOpts[0]
			if got.Agent != tt.want {
				t.Errorf("AddOptions.Agent = %q, want %q", got.Agent, tt.want)
			}
			if got.HookBead != "gt-next" {
				t.Errorf("AddOptions.HookBead = %q, want gt-next: the agent is stored by the write that sets the hook", got.HookBead)
			}
		})
	}
}

// TestSlingAddOptionsCarriesAgent covers the same field on the fresh-allocation
// path, which builds its AddOptions through the same helper: a sling that
// allocates a new polecat records the agent on its agent bead exactly as a
// reuse does (gt-smab3).
func TestSlingAddOptionsCarriesAgent(t *testing.T) {
	t.Parallel()
	reused := slingAddOptions(
		SlingSpawnOptions{HookBead: "gt-next", Agent: "claude-sonnet"},
		"origin/main", "polecat/alpha/gt-old+mud", true)
	if reused.Agent != "claude-sonnet" || reused.HookBead != "gt-next" {
		t.Errorf("reuse AddOptions = %+v, want the sling's agent beside its hook bead", reused)
	}
	if reused.ResumeBranch != "polecat/alpha/gt-old+mud" || !reused.AllowRecovery {
		t.Errorf("reuse AddOptions = %+v, want the resumed branch with recovery allowed", reused)
	}

	fresh := slingAddOptions(SlingSpawnOptions{HookBead: "gt-next", Agent: "claude-sonnet"},
		"origin/main", "", false)
	if fresh.Agent != "claude-sonnet" || fresh.HookBead != "gt-next" {
		t.Errorf("fresh AddOptions = %+v, want the sling's agent beside its hook bead", fresh)
	}
	if fresh.BaseBranch != "origin/main" || fresh.ResumeBranch != "" || fresh.AllowRecovery {
		t.Errorf("fresh AddOptions = %+v, want the base branch, no resume and no recovery", fresh)
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

// createdPolecatFake is the polecat manager a fresh sling reads its new polecat
// back from, recording the removals.
type createdPolecatFake struct {
	polecat *polecat.Polecat
	getErr  error
	removed []string
}

func (f *createdPolecatFake) Get(string) (*polecat.Polecat, error) { return f.polecat, f.getErr }

func (f *createdPolecatFake) Remove(name string, force bool) error {
	if !force {
		return fmt.Errorf("remove of %s was not forced", name)
	}
	f.removed = append(f.removed, name)
	return nil
}

// TestPrepareSlingPolecatRemovesAgentBeadWhenGetFails guards the fresh path's
// read-back: creation has written the agent bead with the hook set, so a Get
// that fails (the nuke-or-reaper race) has to remove the polecat the way a
// failed worktree check does, or the bead stays hooked with no sandbox.
func TestPrepareSlingPolecatRemovesAgentBeadWhenGetFails(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		fake      *createdPolecatFake
		verifyErr error
	}{
		{"read-back fails", &createdPolecatFake{getErr: polecat.ErrPolecatNotFound}, nil},
		{"worktree verification fails", &createdPolecatFake{polecat: &polecat.Polecat{Name: "alpha", ClonePath: "/gone"}}, errors.New("worktree is gone")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readBackCreatedPolecat(tt.fake, "rig", "alpha",
				func(string) error { return tt.verifyErr })
			if err == nil || got != nil {
				t.Fatalf("readBackCreatedPolecat = (%+v, %v), want an error and no polecat", got, err)
			}
			if !slices.Equal(tt.fake.removed, []string{"alpha"}) {
				t.Errorf("removed %v, want exactly [alpha]", tt.fake.removed)
			}
		})
	}
}

func TestReadBackCreatedPolecatKeepsAVerifiedPolecat(t *testing.T) {
	t.Parallel()
	fake := &createdPolecatFake{polecat: &polecat.Polecat{Name: "alpha", ClonePath: "/ok"}}
	got, err := readBackCreatedPolecat(fake, "rig", "alpha", func(string) error { return nil })
	if err != nil || got != fake.polecat {
		t.Fatalf("readBackCreatedPolecat = (%+v, %v), want the polecat", got, err)
	}
	if len(fake.removed) != 0 {
		t.Errorf("removed %v from a healthy sling", fake.removed)
	}
}
