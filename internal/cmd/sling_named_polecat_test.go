package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// namedSlingFake is a polecat manager with several polecats on disk. It
// records every call so a test can prove a named sling touched only the
// polecat it named (gt-2w4f9).
type namedSlingFake struct {
	polecats map[string]*polecat.Polecat // name -> polecat on disk
	idle     *polecat.Polecat            // what FindIdlePolecat would offer
	reuseErr map[string]error            // per-name ReuseIdlePolecat refusal

	findCalls   int
	reuseNames  []string
	addNames    []string
	allocCalls  int
	getNames    []string
	allocatedAs string
}

func (f *namedSlingFake) FindIdlePolecat() (*polecat.Polecat, error) {
	f.findCalls++
	return f.idle, nil
}

func (f *namedSlingFake) ReuseIdlePolecat(name string, opts polecat.AddOptions) (*polecat.Polecat, error) {
	f.reuseNames = append(f.reuseNames, name)
	if err := f.reuseErr[name]; err != nil {
		return nil, err
	}
	return f.polecats[name], nil
}

func (f *namedSlingFake) Get(name string) (*polecat.Polecat, error) {
	f.getNames = append(f.getNames, name)
	p, ok := f.polecats[name]
	if !ok {
		return nil, polecat.ErrPolecatNotFound
	}
	return p, nil
}

func (f *namedSlingFake) AllocateAndAdd(opts polecat.AddOptions) (string, *polecat.Polecat, error) {
	f.allocCalls++
	return f.allocatedAs, &polecat.Polecat{Name: f.allocatedAs}, nil
}

func (f *namedSlingFake) AddNamedWithOptions(name string, opts polecat.AddOptions) (*polecat.Polecat, error) {
	f.addNames = append(f.addNames, name)
	return &polecat.Polecat{Name: name}, nil
}

// gitWorktreeDir returns a directory verifyWorktreeExists accepts.
func gitWorktreeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

func newNamedSlingFake(t *testing.T) *namedSlingFake {
	t.Helper()
	return &namedSlingFake{
		polecats: map[string]*polecat.Polecat{
			"agate":  {Name: "agate", ClonePath: gitWorktreeDir(t), Branch: "polecat/agate/x"},
			"garnet": {Name: "garnet", ClonePath: gitWorktreeDir(t), Branch: "polecat/garnet/x"},
		},
		// The pool would hand out agate: the bug is that it did, for a
		// sling that named garnet.
		idle:     &polecat.Polecat{Name: "agate"},
		reuseErr: map[string]error{},
	}
}

func reuseForNamedSling(t *testing.T, fake *namedSlingFake, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	t.Helper()
	return reuseIdlePolecatForSling(fake, tmux.NewTmux(), &rig.Rig{Name: "rig", Path: t.TempDir()},
		t.TempDir(), "rig", opts, func() {})
}

func assertOnlyNamedTouched(t *testing.T, fake *namedSlingFake, name string) {
	t.Helper()
	if fake.findCalls != 0 {
		t.Errorf("named sling consulted the idle pool (%d FindIdlePolecat calls); it must never pick another polecat", fake.findCalls)
	}
	for _, n := range fake.reuseNames {
		if n != name {
			t.Errorf("named sling for %s reused %s", name, n)
		}
	}
	for _, n := range fake.getNames {
		if n != name {
			t.Errorf("named sling for %s read %s", name, n)
		}
	}
}

// TestNamedSling_ReusesExactlyTheNamedIdlePolecat: gt sling <bead>
// rig/garnet with garnet idle reuses garnet, even though the pool would have
// offered agate first.
func TestNamedSling_ReusesExactlyTheNamedIdlePolecat(t *testing.T) {
	fake := newNamedSlingFake(t)

	info, err := reuseForNamedSling(t, fake, SlingSpawnOptions{Name: "garnet", HookBead: "gt-0cp3", Create: true})
	if err != nil {
		t.Fatalf("named reuse failed: %v", err)
	}
	if info == nil || info.PolecatName != "garnet" {
		t.Fatalf("named sling got %+v; want polecat garnet", info)
	}
	if len(fake.reuseNames) != 1 {
		t.Fatalf("ReuseIdlePolecat calls = %v; want exactly [garnet]", fake.reuseNames)
	}
	assertOnlyNamedTouched(t, fake, "garnet")
}

// TestNamedSling_RefusesIneligibleNamedPolecat: parked, busy, or holding
// local-only work — the named polecat refuses the reuse, and the sling must
// stop with that reason instead of falling back to another polecat or a
// fresh allocation.
func TestNamedSling_RefusesIneligibleNamedPolecat(t *testing.T) {
	for _, tt := range []struct {
		name   string
		reason string
	}{
		{"parked", "parked (operator parked)"},
		{"busy", "not-idle"},
		{"local-only work", "unpushed commits"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newNamedSlingFake(t)
			fake.reuseErr["garnet"] = fmt.Errorf("%w: %s", polecat.ErrPolecatNeedsRecovery, tt.reason)
			if tt.name == "parked" {
				fake.reuseErr["garnet"] = fmt.Errorf("%w: %w: %s", polecat.ErrPolecatNeedsRecovery, polecat.ErrPolecatParked, tt.reason)
			}

			info, err := reuseForNamedSling(t, fake, SlingSpawnOptions{Name: "garnet", HookBead: "gt-0cp3", Create: true})
			if err == nil {
				t.Fatalf("named sling to an ineligible polecat succeeded with %+v; want a refusal", info)
			}
			if info != nil {
				t.Errorf("refusal returned info %+v", info)
			}
			for _, want := range []string{"rig/garnet", tt.reason, "not substituting"} {
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
					t.Errorf("refusal %q does not mention %q", err, want)
				}
			}
			if !errors.Is(err, polecat.ErrPolecatNeedsRecovery) {
				t.Errorf("refusal does not wrap the reuse error: %v", err)
			}
			assertOnlyNamedTouched(t, fake, "garnet")
		})
	}
}

// TestNamedSling_AbsentPolecat: without --create a missing named polecat is
// refused; with --create the reuse step steps aside (nil, nil) so the caller
// creates it — by name, see TestAllocatePolecatForSling_*.
func TestNamedSling_AbsentPolecat(t *testing.T) {
	t.Run("without create refuses", func(t *testing.T) {
		fake := newNamedSlingFake(t)
		info, err := reuseForNamedSling(t, fake, SlingSpawnOptions{Name: "flint", HookBead: "gt-3s52"})
		if err == nil {
			t.Fatalf("named sling to a missing polecat without --create succeeded with %+v", info)
		}
		for _, want := range []string{"rig/flint", "does not exist", "--create"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not mention %q", err, want)
			}
		}
		if len(fake.reuseNames) != 0 {
			t.Errorf("reuse attempted on %v", fake.reuseNames)
		}
		assertOnlyNamedTouched(t, fake, "flint")
	})
	t.Run("with create defers to creation", func(t *testing.T) {
		fake := newNamedSlingFake(t)
		info, err := reuseForNamedSling(t, fake, SlingSpawnOptions{Name: "flint", HookBead: "gt-3s52", Create: true})
		if err != nil || info != nil {
			t.Fatalf("got (%+v, %v); want (nil, nil) so the caller creates flint", info, err)
		}
		if len(fake.reuseNames) != 0 {
			t.Errorf("reuse attempted on %v", fake.reuseNames)
		}
		assertOnlyNamedTouched(t, fake, "flint")
	})
}

// TestNamedSling_LookupFailureRefuses: a polecat that cannot be read is not
// "absent" — refusing is the only answer that cannot substitute.
func TestNamedSling_LookupFailureRefuses(t *testing.T) {
	fake := &namedSlingLookupErrFake{namedSlingFake: newNamedSlingFake(t)}
	info, err := reuseIdlePolecatForSling(fake, tmux.NewTmux(), &rig.Rig{Name: "rig", Path: t.TempDir()},
		t.TempDir(), "rig", SlingSpawnOptions{Name: "garnet", Create: true}, func() {})
	if err == nil || info != nil {
		t.Fatalf("got (%+v, %v); want a refusal", info, err)
	}
	if fake.findCalls != 0 || len(fake.reuseNames) != 0 {
		t.Errorf("lookup failure fell through (find=%d reuse=%v)", fake.findCalls, fake.reuseNames)
	}
}

type namedSlingLookupErrFake struct{ *namedSlingFake }

func (f *namedSlingLookupErrFake) Get(name string) (*polecat.Polecat, error) {
	return nil, errors.New("beads unavailable")
}

// TestAllocatePolecatForSling_NamedCreatesByThatName: --create on an absent
// named polecat creates it under exactly that name, never a pool name.
func TestAllocatePolecatForSling_NamedCreatesByThatName(t *testing.T) {
	fake := newNamedSlingFake(t)
	fake.allocatedAs = "basalt"

	name, err := allocatePolecatForSling(fake, "rig", "flint", polecat.AddOptions{HookBead: "gt-3s52"})
	if err != nil {
		t.Fatalf("allocatePolecatForSling: %v", err)
	}
	if name != "flint" {
		t.Fatalf("created %q; want flint", name)
	}
	if fake.allocCalls != 0 {
		t.Errorf("pool allocation ran %d times for a named create", fake.allocCalls)
	}
	if len(fake.addNames) != 1 || fake.addNames[0] != "flint" {
		t.Errorf("AddWithOptions calls = %v; want [flint]", fake.addNames)
	}
}

// TestAllocatePolecatForSling_UnnamedUsesPool keeps the rig-target path.
func TestAllocatePolecatForSling_UnnamedUsesPool(t *testing.T) {
	fake := newNamedSlingFake(t)
	fake.allocatedAs = "basalt"

	name, err := allocatePolecatForSling(fake, "rig", "", polecat.AddOptions{})
	if err != nil {
		t.Fatalf("allocatePolecatForSling: %v", err)
	}
	if name != "basalt" || fake.allocCalls != 1 || len(fake.addNames) != 0 {
		t.Fatalf("name=%q alloc=%d add=%v; want pool allocation of basalt", name, fake.allocCalls, fake.addNames)
	}
}

// TestAllocatePolecatForSling_NamedCreateRaceRefuses: if the name was taken
// between the lookup and the create, refuse rather than pick another.
func TestAllocatePolecatForSling_NamedCreateRaceRefuses(t *testing.T) {
	fake := &namedSlingAddErrFake{namedSlingFake: newNamedSlingFake(t)}
	_, err := allocatePolecatForSling(fake, "rig", "flint", polecat.AddOptions{})
	if !errors.Is(err, polecat.ErrPolecatExists) {
		t.Fatalf("err = %v; want ErrPolecatExists", err)
	}
	if fake.allocCalls != 0 {
		t.Errorf("named create fell back to pool allocation")
	}
}

type namedSlingAddErrFake struct{ *namedSlingFake }

func (f *namedSlingAddErrFake) AddNamedWithOptions(name string, opts polecat.AddOptions) (*polecat.Polecat, error) {
	return nil, polecat.ErrPolecatExists
}

// TestResolveTarget_DeadNamedPolecatKeepsItsName: the dead-polecat fallback
// in resolveTarget must hand the named polecat to the spawn, for both target
// forms. It used to pass only the rig, so the pool picked (gt-2w4f9, gt-n2gi).
func TestResolveTarget_DeadNamedPolecatKeepsItsName(t *testing.T) {
	for _, tt := range []struct {
		target string
		create bool
	}{
		{"gastown/polecats/garnet", false},
		{"gastown/polecats/garnet", true},
		{"gastown/garnet", true},
	} {
		t.Run(fmt.Sprintf("%s create=%v", tt.target, tt.create), func(t *testing.T) {
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0o755); err != nil {
				t.Fatal(err)
			}
			prevResolve := resolveTargetAgentFn
			prevSpawn := spawnPolecatForSling
			t.Cleanup(func() {
				resolveTargetAgentFn = prevResolve
				spawnPolecatForSling = prevSpawn
			})
			resolveTargetAgentFn = func(string) (string, string, string, error) {
				return "", "", "", errors.New("no session")
			}
			var got SlingSpawnOptions
			spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
				got = opts
				return &SpawnedPolecatInfo{RigName: rigName, PolecatName: opts.Name}, nil
			}

			res, err := resolveTarget(tt.target, ResolveTargetOptions{Create: tt.create, NoBoot: true, TownRoot: townRoot})
			if err != nil {
				t.Fatalf("resolveTarget: %v", err)
			}
			if got.Name != "garnet" {
				t.Fatalf("spawn Name = %q; want garnet", got.Name)
			}
			if got.Create != tt.create {
				t.Fatalf("spawn Create = %v; want %v", got.Create, tt.create)
			}
			if res.Agent != "gastown/polecats/garnet" {
				t.Fatalf("Agent = %q", res.Agent)
			}
		})
	}
}

// TestNamedPolecatRefusal_HintPerCause: the refusal's hint must fit the
// cause. A parked polecat is resumed with gt agent resume; a polecat already
// holding the slung bead resumes that session; any other refusal must not
// suggest hooking the new bead onto a polecat that holds other work.
func TestNamedPolecatRefusal_HintPerCause(t *testing.T) {
	needsRecovery := func(reason string) error {
		return fmt.Errorf("%w: %s", polecat.ErrPolecatNeedsRecovery, reason)
	}
	for _, tt := range []struct {
		name      string
		heldIssue string
		err       error
		want      []string
		notWant   []string
	}{
		{
			name:    "parked",
			err:     fmt.Errorf("%w: %w: parked (operator parked)", polecat.ErrPolecatNeedsRecovery, polecat.ErrPolecatParked),
			want:    []string{"gt agent resume rig/garnet", "gt sling gt-new rig/polecats/garnet", "gt sling gt-new rig"},
			notWant: []string{"gt session start", "gt sling gt-new rig/garnet"},
		},
		{
			name:      "holds the slung bead",
			heldIssue: "gt-new",
			err:       needsRecovery("unpushed commits"),
			want:      []string{"gt session start rig/garnet --issue gt-new"},
			notWant:   []string{"gt agent resume"},
		},
		{
			name:      "holds other work",
			heldIssue: "gt-other",
			err:       needsRecovery("unpushed commits"),
			want:      []string{"It holds gt-other", "gt polecat check-recovery rig/garnet", "gt sling gt-new rig"},
			notWant:   []string{"gt session start", "--issue gt-new", "gt agent resume"},
		},
		{
			name:    "busy",
			err:     needsRecovery("not-idle"),
			want:    []string{"gt polecat check-recovery rig/garnet"},
			notWant: []string{"gt session start", "gt agent resume"},
		},
		{
			name:    "broken worktree",
			err:     errors.New("idle polecat worktree not found at /x: no such file"),
			want:    []string{"gt polecat check-recovery rig/garnet"},
			notWant: []string{"gt session start", "gt agent resume"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			msg := namedPolecatRefusal("rig", "garnet", "gt-new", tt.heldIssue, tt.err).Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("refusal lacks %q:\n%s", w, msg)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(msg, nw) {
					t.Errorf("refusal must not contain %q:\n%s", nw, msg)
				}
			}
			if strings.Count(msg, "rig/garnet cannot take") != 1 {
				t.Errorf("polecat named more than once in the headline:\n%s", msg)
			}
		})
	}
}

// TestNamedSling_HeldIssueReachesTheHint: the reuse path reads the named
// polecat's current work and hands it to the refusal.
func TestNamedSling_HeldIssueReachesTheHint(t *testing.T) {
	fake := newNamedSlingFake(t)
	fake.polecats["garnet"].Issue = "gt-0cp3"
	fake.reuseErr["garnet"] = fmt.Errorf("%w: unpushed commits", polecat.ErrPolecatNeedsRecovery)

	_, err := reuseForNamedSling(t, fake, SlingSpawnOptions{Name: "garnet", HookBead: "gt-0cp3"})
	if err == nil || !strings.Contains(err.Error(), "gt session start rig/garnet --issue gt-0cp3") {
		t.Fatalf("refusal for garnet's own bead does not offer to resume it: %v", err)
	}
}

// TestResolveTarget_NamedPolecatTargetRefusals covers targets that must not
// reach the spawn: a malformed or empty name (which would otherwise fall back
// to the pool), and the <rig>/<name> shorthand without --create, which stays
// a resolve error as before.
func TestResolveTarget_NamedPolecatTargetRefusals(t *testing.T) {
	for _, tt := range []struct {
		target string
		create bool
		want   string
	}{
		{"gastown/polecats/", true, "invalid polecat name"},
		{"gastown/polecats/", false, "invalid polecat name"},
		{"gastown/polecats/..", true, "invalid polecat name"},
		{"gastown/polecats/garnet/extra", true, "invalid polecat target"},
		{"gastown/garnet", false, "resolving target"},
	} {
		t.Run(fmt.Sprintf("%s create=%v", tt.target, tt.create), func(t *testing.T) {
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0o755); err != nil {
				t.Fatal(err)
			}
			prevResolve := resolveTargetAgentFn
			prevSpawn := spawnPolecatForSling
			t.Cleanup(func() {
				resolveTargetAgentFn = prevResolve
				spawnPolecatForSling = prevSpawn
			})
			resolveTargetAgentFn = func(string) (string, string, string, error) {
				return "", "", "", errors.New("no session")
			}
			spawned := false
			spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
				spawned = true
				return nil, errors.New("unexpected spawn")
			}

			_, err := resolveTarget(tt.target, ResolveTargetOptions{Create: tt.create, NoBoot: true, TownRoot: townRoot})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v; want it to contain %q", err, tt.want)
			}
			if spawned {
				t.Fatal("spawn was called for a target that must be refused")
			}
		})
	}
}

// TestResolveTarget_NamedRefusalNotDoubled: resolveTarget's wrap must not
// repeat the polecat the refusal already names.
func TestResolveTarget_NamedRefusalNotDoubled(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevResolve := resolveTargetAgentFn
	prevSpawn := spawnPolecatForSling
	t.Cleanup(func() {
		resolveTargetAgentFn = prevResolve
		spawnPolecatForSling = prevSpawn
	})
	resolveTargetAgentFn = func(string) (string, string, string, error) {
		return "", "", "", errors.New("no session")
	}
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		return nil, namedPolecatRefusal(rigName, opts.Name, "gt-new", "",
			fmt.Errorf("%w: not-idle", polecat.ErrPolecatNeedsRecovery))
	}

	_, err := resolveTarget("gastown/polecats/garnet", ResolveTargetOptions{NoBoot: true, TownRoot: townRoot})
	if err == nil {
		t.Fatal("expected the refusal")
	}
	if strings.Count(strings.SplitN(err.Error(), "\n", 2)[0], "garnet") != 1 {
		t.Fatalf("first line names the polecat more than once: %q", strings.SplitN(err.Error(), "\n", 2)[0])
	}
}

// brokenIdleReclaimerFake records whether the pre-allocation reclaim sweep ran.
type brokenIdleReclaimerFake struct {
	listCalls int
	reclaimed []string
	polecats  []*polecat.Polecat
}

func (f *brokenIdleReclaimerFake) List() ([]*polecat.Polecat, error) {
	f.listCalls++
	return f.polecats, nil
}

func (f *brokenIdleReclaimerFake) ReclaimBrokenIdlePolecat(name string) error {
	f.reclaimed = append(f.reclaimed, name)
	return nil
}

// TestReclaimBrokenIdleUnlessNamed: a named sling must not sweep other
// polecats; a rig sling still does.
func TestReclaimBrokenIdleUnlessNamed(t *testing.T) {
	broken := &polecat.Polecat{Name: "agate", State: polecat.StateIdle, ClonePath: filepath.Join(t.TempDir(), "gone")}

	named := &brokenIdleReclaimerFake{polecats: []*polecat.Polecat{broken}}
	reclaimBrokenIdleUnlessNamed(named, SlingSpawnOptions{Name: "garnet"})
	if named.listCalls != 0 || len(named.reclaimed) != 0 {
		t.Fatalf("named sling swept the pool: list=%d reclaimed=%v", named.listCalls, named.reclaimed)
	}

	rigSling := &brokenIdleReclaimerFake{polecats: []*polecat.Polecat{broken}}
	reclaimBrokenIdleUnlessNamed(rigSling, SlingSpawnOptions{})
	if rigSling.listCalls != 1 || len(rigSling.reclaimed) != 1 || rigSling.reclaimed[0] != "agate" {
		t.Fatalf("rig sling did not sweep: list=%d reclaimed=%v", rigSling.listCalls, rigSling.reclaimed)
	}
}
