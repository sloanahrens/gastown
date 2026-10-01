package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/land"
)

// These tests drive runSling's decisions through slingHarness: what a sling
// refuses, what it writes and in which order, and what it undoes when a step
// after the spawn fails. bd, tmux, git and mail are fakes; the helpers they
// stand in for have their own tests.

const slingBead = "gt-abc123"

func TestSlingRefusesPolecatsByRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		env    map[string]string
		refuse bool
	}{
		{"polecat role", map[string]string{"GT_ROLE": "polecat"}, true},
		{"compound polecat role", map[string]string{"GT_ROLE": "gastown/polecats/Toast"}, true},
		{"no role, polecat name", map[string]string{"GT_POLECAT": "Toast"}, true},
		// GH #664: a coordinator keeps a stale GT_POLECAT from spawning one.
		{"mayor with stale GT_POLECAT", map[string]string{"GT_ROLE": "mayor", "GT_POLECAT": "Toast"}, false},
		{"crew", map[string]string{"GT_ROLE": "gastown/crew/sloan"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.env = tc.env
			h.addBead(slingBead, beadInfo{})
			err := h.sling(slingBead, "gastown")
			if tc.refuse {
				wantSlingErr(t, err, "polecats cannot sling")
				if len(h.log()) != 0 {
					t.Errorf("a refused polecat sling did work: %q", h.log())
				}
				return
			}
			if err != nil {
				t.Fatalf("sling: %v", err)
			}
		})
	}
}

func TestSlingResumeFlags(t *testing.T) {
	t.Parallel()
	t.Run("branch and pr are exclusive", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.run.opts.resumeBranch, h.run.opts.resumePR = "fix/x", 7
		wantSlingErr(t, h.sling(slingBead, "gastown"), "mutually exclusive")
	})
	t.Run("base branch with a resume", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.run.opts.resumeBranch, h.run.opts.baseBranch = "fix/x", "develop"
		wantSlingErr(t, h.sling(slingBead, "gastown"), "--base-branch cannot be combined")
	})
	t.Run("pr resolves to the branch the polecat resumes", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.opts.resumePR = 42
		h.run.resolvePRBranch = func(pr int) (string, error) { return "feature/pr-42", nil }
		var got ResolveTargetOptions
		h.run.resolveTarget = func(target string, opts ResolveTargetOptions) (*ResolvedTarget, error) {
			got = opts
			return h.resolveTarget(target, opts)
		}
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		if got.ResumeBranch != "feature/pr-42" {
			t.Errorf("resolveTarget ResumeBranch = %q, want feature/pr-42", got.ResumeBranch)
		}
		h.wantCalls("instantiate", "instantiate mol-polecat-work on gt-abc123 vars=resume_branch=feature/pr-42")
	})
}

// TestSlingRefusesUnslingableBeads: each guard refuses before the target is
// resolved, so a refusal never spawns a polecat.
func TestSlingRefusesUnslingableBeads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		bead  beadInfo
		force bool
		want  string // "" = the sling goes ahead
	}{
		{name: "closed", bead: beadInfo{Status: "closed"}, want: "work already completed"},
		{name: "closed under force", bead: beadInfo{Status: "closed"}, force: true, want: "work already completed"},
		{name: "tombstone", bead: beadInfo{Status: "tombstone"}, want: "is tombstone"},
		{name: "flag-like title", bead: beadInfo{Title: "--help"}, want: "looks like a CLI flag"},
		{name: "deferred status", bead: beadInfo{Status: "deferred"}, want: "refusing to sling deferred bead"},
		{name: "deferred in description", bead: beadInfo{Description: "Deferred to post-launch."}, want: "refusing to sling deferred bead"},
		{name: "deferred under force", bead: beadInfo{Status: "deferred"}, force: true},
		{name: "operator label", bead: beadInfo{Labels: []string{dispatch.OperatorLabel}}, want: dispatch.SlingRefusalMarker},
		{name: "person assignee", bead: beadInfo{Assignee: "sloan"}, want: "is the operator's work"},
		{name: "operator label under force", bead: beadInfo{Labels: []string{dispatch.OperatorLabel}}, force: true},
		{name: "submitted for landing", bead: beadInfo{Labels: []string{land.LabelReadyToLand}}, want: "submitted for landing"},
		{name: "submitted for landing under force", bead: beadInfo{Labels: []string{land.LabelReadyToLand}}, force: true, want: "the landing worker owns it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, tc.bead)
			h.run.opts.force = tc.force
			err := h.sling(slingBead, "gastown")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("sling: %v", err)
				}
				return
			}
			wantSlingErr(t, err, tc.want)
			h.wantNo("resolve target")
			h.wantNo("hook")
		})
	}
}

// TestSlingAlreadyAssigned: re-slinging assigned work is a no-op to the same
// target, an error to another, and a reassignment under --force or when the
// holder's session is dead.
func TestSlingAlreadyAssigned(t *testing.T) {
	t.Parallel()
	const holder = "gastown/polecats/Nux"
	t.Run("same target is a no-op", func(t *testing.T) {
		t.Parallel()
		for _, status := range []string{"hooked", "pinned"} {
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{Status: status, Assignee: holder})
			if err := h.sling(slingBead, holder); err != nil {
				t.Fatalf("%s: sling: %v", status, err)
			}
			if !strings.Contains(h.out.String(), "no-op") {
				t.Errorf("%s: output %q does not report a no-op", status, h.out.String())
			}
			h.wantNo("resolve target")
			h.wantNo("hook")
		}
	})
	t.Run("rig target matches its polecat", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: holder})
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantNo("resolve target")
	})
	t.Run("self target matches the caller", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "pinned", Assignee: "mayor/"})
		h.run.resolveSelf = func() (string, string, string, error) { return "mayor/", "%0", slingTestTown, nil }
		if err := h.sling(slingBead, "."); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantNo("resolve target")
	})
	t.Run("other target is refused", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "in_progress", Assignee: holder})
		wantSlingErr(t, h.sling(slingBead, "gastown/crew/sloan"), "already in_progress to "+holder)
		h.wantNo("resolve target")
	})
	t.Run("force reassigns from a live polecat", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: holder})
		h.run.opts.force = true
		if err := h.sling(slingBead, holder); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("clear reassigned", "clear reassigned "+holder)
		h.wantCalls("unhook", "unhook "+slingBead)
		h.wantCalls("hook", "hook gt-abc123 "+holder)
		h.wantNo("survival guard")
	})
	t.Run("dead holder is auto-forced after the survival guard", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: holder})
		h.dead[holder] = true
		if err := h.sling(slingBead, holder); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("survival guard", "survival guard gt-abc123 "+holder)
		h.wantCalls("hook", "hook gt-abc123 "+holder)
		if !strings.Contains(h.out.String(), "auto-forcing re-sling") {
			t.Errorf("output %q does not report the auto-force", h.out.String())
		}
	})
	t.Run("dead holder whose work survives is refused", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: holder})
		h.dead[holder] = true
		h.run.survivingWorkGuard = func(_, _, _ string) error { return &reslingRefusal{msg: "work survives on polecat/Nux"} }
		err := h.sling(slingBead, "gastown")
		wantSlingErr(t, err, "work survives")
		if !errors.Is(err, errReslingRefused) {
			t.Errorf("refusal %v is not errReslingRefused", err)
		}
		h.wantNo("resolve target")
	})
	t.Run("dead holder with --branch resumes without the guard", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: holder})
		h.dead[holder] = true
		h.run.opts.resumeBranch = "polecat/Nux/x"
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantNo("survival guard")
	})
	t.Run("formula on the same target still runs the formula", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: holder})
		h.formulas["mol-review"] = true
		h.run.opts.on = slingBead
		if err := h.sling("mol-review", holder); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("instantiate", "instantiate mol-review on gt-abc123 vars=")
		h.wantNo("unhook")
	})
}

// TestSlingRollsBackOnEveryPostSpawnExit (gt-7evi4): once resolveTarget has
// spawned a polecat, every failure short of the commit point rolls it back
// exactly once. The rollback names the bead only once the sling has written
// to it, and closes the auto-convoy only when raw metadata failed (gt-yg24).
func TestSlingRollsBackOnEveryPostSpawnExit(t *testing.T) {
	t.Parallel()
	injected := errors.New("injected failure")
	cases := []struct {
		name         string
		inject       func(h *slingHarness)
		wantErr      string // "" = success
		wantRollback string
	}{
		{name: "molecule bond read fails", wantErr: "checking existing molecule bonds", wantRollback: "rollback Toast bead= convoy=",
			inject: func(h *slingHarness) {
				h.run.collectMolecules = func(*beadInfo, string, string) ([]string, error) { return nil, injected }
			}},
		{name: "stale molecule burn fails", wantErr: "burning stale molecules", wantRollback: "rollback Toast bead= convoy=",
			inject: func(h *slingHarness) {
				h.molecules[slingBead] = []string{"gt-wisp-old"}
				h.run.burnMolecules = func([]string, string, string) error { return injected }
			}},
		{name: "live molecule refuses re-sling", wantErr: "already has 1 attached molecule", wantRollback: "rollback Toast bead= convoy=",
			inject: func(h *slingHarness) {
				h.addBead(slingBead, beadInfo{Status: "blocked"}) // unassigned + blocked: not an orphan
				h.molecules[slingBead] = []string{"gt-wisp-old"}
			}},
		{name: "cross-rig guard fails", wantErr: "cross-rig", wantRollback: "rollback Toast bead= convoy=",
			inject: func(h *slingHarness) {
				h.run.crossRigGuard = func(string, string, string) error { return errors.New("cross-rig mismatch") }
			}},
		{name: "formula instantiation fails", wantErr: "instantiating formula", wantRollback: "rollback Toast bead=gt-abc123 convoy=",
			inject: func(h *slingHarness) {
				h.run.instantiateFormula = func(_ context.Context, _, _, _, _, _ string, _ []string) (*FormulaOnBeadResult, error) {
					return nil, injected
				}
			}},
		{name: "assignee lock fails", wantErr: "serializing hook write", wantRollback: "rollback Toast bead=gt-abc123 convoy=",
			inject: func(h *slingHarness) {
				h.run.lockAssignee = func(string, string) (func(), error) { return nil, injected }
			}},
		{name: "raw metadata store fails closes the convoy", wantErr: "storing raw sling metadata", wantRollback: "rollback Toast bead=gt-abc123 convoy=hq-cv-auto",
			inject: func(h *slingHarness) {
				h.run.opts.hookRawBead, h.run.opts.noMerge, h.run.opts.noConvoy = true, true, false
				h.run.storeFields = func(string, string, beadFieldUpdates) error { return injected }
			}},
		{name: "hook fails keeps the auto-convoy", wantErr: "injected failure", wantRollback: "rollback Toast bead=gt-abc123 convoy=",
			inject: func(h *slingHarness) {
				h.run.opts.hookRawBead, h.run.opts.noConvoy = true, false
				h.run.hook = func(string, string, string, string) error { return injected }
			}},
		{name: "session start fails", wantErr: "starting polecat session", wantRollback: "rollback Toast bead=gt-abc123 convoy=",
			inject: func(h *slingHarness) {
				h.run.startSession = func(*SpawnedPolecatInfo) (string, error) { return "", injected }
			}},
		{name: "success commits", inject: func(*slingHarness) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{})
			tc.inject(h)
			err := h.sling(slingBead, "gastown")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("sling: %v", err)
				}
				h.wantNo("rollback")
				return
			}
			wantSlingErr(t, err, tc.wantErr)
			h.wantCalls("rollback", tc.wantRollback)
			if strings.Contains(tc.wantRollback, "bead=gt-") {
				h.wantCalls("restore raw fields", "restore raw fields "+slingBead)
			} else {
				h.wantNo("restore raw fields")
			}
		})
	}
}

// TestSlingForcedPinnedRollbackRestoresThePin: a forced sling of a pinned
// bead that fails after writing puts the pin back.
func TestSlingForcedPinnedRollbackRestoresThePin(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{Status: "pinned", Assignee: "mayor/"})
	h.run.opts.force = true
	h.run.hook = func(string, string, string, string) error { return errors.New("injected") }
	wantSlingErr(t, h.sling(slingBead, "gastown"), "injected")
	h.wantCalls("restore pinned", "restore pinned gt-abc123 mayor/")
}

// TestSlingDryRunWritesNothing: a dry run resolves the target and reports,
// but looks up no convoy (GH#3903), writes nothing and rolls nothing back.
func TestSlingDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{})
	h.run.opts.dryRun = true
	h.run.opts.noConvoy = false
	if err := h.sling(slingBead, "gastown"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, p := range []string{"convoy lookup", "create convoy", "instantiate", "store fields", "hook", "rollback", "admit", "start session", "nudge"} {
		h.wantNo(p)
	}
	if !strings.Contains(h.out.String(), "Would create convoy") {
		t.Errorf("dry run output %q does not describe the convoy", h.out.String())
	}
}

// TestSlingWritesInOrder: the success path of a bare bead to a rig. The
// convoy exists before the formula, the hook comes after the formula and
// any raw metadata, and the session starts last (gt-jn40ft).
func TestSlingWritesInOrder(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{})
	h.run.opts.noConvoy = false
	if err := h.sling(slingBead, "gastown"); err != nil {
		t.Fatalf("sling: %v", err)
	}
	want := []string{
		"autocommit off",
		"lock bead gt-abc123",
		`resolve target "gastown"`,
		"admit gastown gt-abc123",
		"cross-rig guard gt-abc123 gastown/polecats/Toast",
		"convoy lookup gt-abc123",
		"create convoy gt-abc123",
		"instantiate mol-polecat-work on gt-abc123 vars=",
		"lock assignee gastown/polecats/Toast",
		"reassign gt-abc123  -> gastown/polecats/Toast",
		"hook gt-abc123 gastown/polecats/Toast",
		"clear orphan labels gt-abc123",
		"feed sling",
		"agent hook gastown/polecats/Toast gt-abc123",
		"store fields gt-abc123",
		"start session Toast",
		"unlock assignee gastown/polecats/Toast",
		"unlock bead gt-abc123",
		"autocommit restore",
	}
	if got := h.log(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("call log:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	stored := h.stored[slingBead]
	if len(stored) != 1 || stored[0].AttachedMolecule != "gt-wisp-new" || stored[0].AttachedFormula != "mol-polecat-work" || stored[0].ConvoyID != "hq-cv-auto" {
		t.Errorf("stored fields = %+v, want the wisp, formula and convoy attached", stored)
	}
}

func TestSlingAutoConvoy(t *testing.T) {
	t.Parallel()
	t.Run("already tracked", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.opts.noConvoy = false
		h.convoys[slingBead] = "hq-cv-old"
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantNo("create convoy")
	})
	t.Run("no-convoy", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.opts.noConvoy = true
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantNo("convoy lookup")
	})
	t.Run("create failure is not fatal", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.opts.noConvoy = false
		h.run.createConvoy = func(string, string, string, bool, string, string, string, string) (string, error) {
			return "", errors.New("dolt busy")
		}
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("hook", "hook gt-abc123 gastown/polecats/Toast")
	})
}

// TestSlingFormulaChoice (#288): a bare bead to a polecat gets
// mol-polecat-work unless --hook-raw-bead, --formula overrides it, and a
// non-polecat target gets no formula.
func TestSlingFormulaChoice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		target  string
		opts    func(o *slingOptions)
		formula string
	}{
		{"default", "gastown", func(*slingOptions) {}, "mol-polecat-work"},
		{"explicit", "gastown", func(o *slingOptions) { o.formula = "mol-fix" }, "mol-fix"},
		{"raw", "gastown", func(o *slingOptions) { o.hookRawBead = true }, ""},
		{"crew target", "gastown/crew/sloan", func(*slingOptions) {}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{})
			tc.opts(&h.run.opts)
			if err := h.sling(slingBead, tc.target); err != nil {
				t.Fatalf("sling: %v", err)
			}
			if tc.formula == "" {
				h.wantNo("instantiate")
				return
			}
			h.wantCalls("instantiate", "instantiate "+tc.formula+" on gt-abc123 vars=")
		})
	}
}

// TestSlingMoleculeGuard: an attached molecule refuses a plain re-sling, is
// burned under --force or when it is an orphan, and a dry run only reports.
func TestSlingMoleculeGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		bead  beadInfo
		force bool
		dead  bool
		burn  bool
		err   string
	}{
		{name: "orphan on an open bead", bead: beadInfo{}, burn: true},
		{name: "live on a blocked bead", bead: beadInfo{Status: "blocked"}, err: "already has 1 attached molecule"},
		{name: "live under force", bead: beadInfo{Status: "blocked"}, force: true, burn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, tc.bead)
			h.molecules[slingBead] = []string{"gt-wisp-old"}
			h.run.opts.force = tc.force
			err := h.sling(slingBead, "gastown")
			if tc.err != "" {
				wantSlingErr(t, err, tc.err)
				h.wantNo("burn")
				return
			}
			if err != nil {
				t.Fatalf("sling: %v", err)
			}
			h.wantCalls("burn", "burn gt-abc123 gt-wisp-old")
		})
	}
}

// TestSlingStoresRequestInBead: the flags a polecat reads back are stored on
// the bead. Raw review-only and no-merge work stores them before the hook,
// so the polecat never sees a hooked bead without them.
func TestSlingStoresRequestInBead(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{})
	h.run.opts.hookRawBead, h.run.opts.noMerge, h.run.opts.reviewOnly, h.run.opts.ralph = true, true, true, true
	h.run.opts.argsText = "patch release"
	if err := h.sling(slingBead, "gastown"); err != nil {
		t.Fatalf("sling: %v", err)
	}
	log := strings.Join(h.log(), "\n")
	if i, j := strings.Index(log, "store fields"), strings.Index(log, "hook gt-"); i < 0 || j < 0 || i > j {
		t.Errorf("raw metadata was not stored before the hook:\n%s", log)
	}
	h.wantCalls("agent mode", "agent mode gastown/polecats/Toast ralph")
	stored := h.stored[slingBead]
	if len(stored) != 2 {
		t.Fatalf("stored %d field updates, want 2 (before and after the hook)", len(stored))
	}
	u := stored[1]
	if !u.NoMerge || !u.ReviewOnly || u.Args != "patch release" || u.Mode == nil || *u.Mode != "ralph" || u.Dispatcher != "mayor" {
		t.Errorf("stored fields = %+v", u)
	}
}

// TestSlingNudgesTheTarget: an existing agent is nudged in its pane, the
// mayor also gets a queued hook notice, a fresh polecat and a self-sling are
// not nudged.
func TestSlingNudgesTheTarget(t *testing.T) {
	t.Parallel()
	t.Run("crew", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		if err := h.sling(slingBead, "gastown/crew/sloan"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("nudge", "nudge %9 gt-abc123")
		h.wantNo("queue nudge")
	})
	t.Run("mayor", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		if err := h.sling(slingBead, "mayor/"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("nudge", "nudge %9 gt-abc123")
		h.wantCalls("queue nudge", "queue nudge hq-mayor: Hook updated: attached bead gt-abc123")
	})
	t.Run("fresh polecat", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantNo("nudge")
	})
	t.Run("self", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.resolveSelf = func() (string, string, string, error) { return "gastown/crew/sloan", "%0", slingTestTown, nil }
		if err := h.sling(slingBead); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantNo("nudge")
		h.wantCalls("hook", "hook gt-abc123 gastown/crew/sloan")
	})
}

// TestSlingRoutesRequests: runSling hands batches, schedules, convoys, epics
// and standalone formulas to their own paths.
func TestSlingRoutesRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		setup    func(h *slingHarness)
		want     string // the one routing call
		wantErr  string
		wantNone bool
	}{
		{name: "batch to a rig", args: []string{"gt-a", "gt-b", "gt-c", "gastown"}, want: "batch sling gt-a,gt-b,gt-c -> gastown"},
		{name: "batch auto-resolves its rig", args: []string{"gt-a", "gt-b", "gt-c"}, want: "batch sling gt-a,gt-b,gt-c -> gastown"},
		{name: "two beads auto-resolve", args: []string{"gt-a", "gt-b"}, want: "batch sling gt-a,gt-b -> gastown"},
		{name: "convoy", args: []string{"hq-cv-1"}, want: "convoy sling hq-cv-1",
			setup: func(h *slingHarness) { h.run.idType = func(string) (string, error) { return "convoy", nil } }},
		{name: "epic", args: []string{"gt-epic"}, want: "epic sling gt-epic",
			setup: func(h *slingHarness) { h.run.idType = func(string) (string, error) { return "epic", nil } }},
		{name: "standalone formula", args: []string{"mol-patrol", "gastown"}, want: "sling formula mol-patrol gastown",
			setup: func(h *slingHarness) { h.formulas["mol-patrol"] = true }},
		{name: "deferred batch", args: []string{"gt-a", "gt-b", "gt-c", "gastown"}, want: "batch schedule gt-a,gt-b,gt-c -> gastown",
			setup: func(h *slingHarness) { h.run.shouldDefer = func() (bool, error) { return true, nil } }},
		{name: "deferred bead to a rig", args: []string{slingBead, "gastown"}, want: "schedule gt-abc123 -> gastown formula=mol-polecat-work",
			setup: func(h *slingHarness) {
				h.addBead(slingBead, beadInfo{})
				h.run.shouldDefer = func() (bool, error) { return true, nil }
			}},
		// gh#3917: a standalone formula consumes no scheduler slot.
		{name: "deferred standalone formula", args: []string{"mol-patrol", "gastown"}, want: "sling formula mol-patrol gastown",
			setup: func(h *slingHarness) {
				h.formulas["mol-patrol"] = true
				h.run.shouldDefer = func() (bool, error) { return true, nil }
			}},
		{name: "deferred convoy", args: []string{"hq-cv-1"}, want: "convoy schedule hq-cv-1",
			setup: func(h *slingHarness) {
				h.run.shouldDefer = func() (bool, error) { return true, nil }
				h.run.idType = func(string) (string, error) { return "convoy", nil }
			}},
		{name: "deferred formula on a bead", args: []string{"mol-review", "gastown"}, want: "schedule gt-abc123 -> gastown formula=mol-review",
			setup: func(h *slingHarness) {
				h.run.shouldDefer = func() (bool, error) { return true, nil }
				h.run.opts.on = slingBead
			}},
		{name: "deferred non-rig target", args: []string{slingBead, "gastown/crew/sloan"}, wantErr: "deferred dispatch requires a rig target",
			setup: func(h *slingHarness) { h.run.shouldDefer = func() (bool, error) { return true, nil } }},
		{name: "deferred task without a rig", args: []string{slingBead}, wantErr: "deferred dispatch requires a rig target",
			setup: func(h *slingHarness) { h.run.shouldDefer = func() (bool, error) { return true, nil } }},
		{name: "deferred epic with a rig", args: []string{"gt-epic", "gastown"}, wantErr: "cannot be scheduled with an explicit rig",
			setup: func(h *slingHarness) {
				h.run.shouldDefer = func() (bool, error) { return true, nil }
				h.run.idType = func(string) (string, error) { return "epic", nil }
			}},
		{name: "neither bead nor formula", args: []string{"Not A Bead"}, wantErr: "is not a valid bead or formula"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			err := h.sling(tc.args...)
			if tc.wantErr != "" {
				wantSlingErr(t, err, tc.wantErr)
				h.wantNo("resolve target")
				return
			}
			if err != nil {
				t.Fatalf("sling: %v", err)
			}
			var routed []string
			for _, p := range []string{"batch", "schedule", "convoy", "epic", "sling formula"} {
				routed = append(routed, h.matching(p)...)
			}
			if len(routed) != 1 || routed[0] != tc.want {
				t.Errorf("routed to %q, want exactly %q", routed, tc.want)
			}
			h.wantNo("resolve target")
		})
	}
}

// TestSlingInputNormalization: trailing slashes, --crew and --stdin shape
// the request before it is routed.
func TestSlingInputNormalization(t *testing.T) {
	t.Parallel()
	t.Run("trailing slash on the target", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		if err := h.sling(slingBead, "gastown/"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("resolve target", `resolve target "gastown"`)
	})
	t.Run("crew flag", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.opts.crew = "mel"
		if err := h.sling(slingBead, "gastown"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		h.wantCalls("resolve target", `resolve target "gastown/crew/mel"`)
	})
	t.Run("crew flag needs a rig", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.run.opts.crew = "mel"
		wantSlingErr(t, h.sling(slingBead), "--crew requires a rig target")
	})
	t.Run("stdin fills --args", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.opts.stdin = true
		h.run.stdin = strings.NewReader("do the `thing`\n")
		if err := h.sling(slingBead, "gastown/crew/sloan"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		if got := h.stored[slingBead][0].Args; got != "do the `thing`" {
			t.Errorf("stored args %q", got)
		}
	})
	t.Run("stdin with --args fills --message", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.addBead(slingBead, beadInfo{})
		h.run.opts.stdin, h.run.opts.argsText = true, "cli args"
		h.run.stdin = strings.NewReader("from stdin")
		if err := h.sling(slingBead, "gastown/crew/sloan"); err != nil {
			t.Fatalf("sling: %v", err)
		}
		if h.run.opts.message != "from stdin" || h.stored[slingBead][0].Args != "cli args" {
			t.Errorf("message %q, stored args %q", h.run.opts.message, h.stored[slingBead][0].Args)
		}
	})
	t.Run("stdin with both set", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		h.run.opts.stdin, h.run.opts.argsText, h.run.opts.message = true, "a", "m"
		wantSlingErr(t, h.sling(slingBead), "cannot use --stdin")
	})
	t.Run("invalid target", func(t *testing.T) {
		t.Parallel()
		h := newSlingHarness(t)
		wantSlingErr(t, h.sling(slingBead, "gastown//polecats"), "empty path segment")
	})
}

// TestSlingClearsOrphanEpisodeLabelsOnHook (gt-vm5g4): a sling that hooks
// the bead ends any witness orphan episode, once, after the hook lands.
func TestSlingClearsOrphanEpisodeLabelsOnHook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		inject func(h *slingHarness)
		clears bool
	}{
		{"success", func(*slingHarness) {}, true},
		{"session fails after the hook", func(h *slingHarness) {
			h.run.startSession = func(*SpawnedPolecatInfo) (string, error) { return "", errors.New("injected") }
		}, true},
		{"hook fails", func(h *slingHarness) {
			h.run.hook = func(string, string, string, string) error { return errors.New("injected") }
		}, false},
		{"fails before the hook", func(h *slingHarness) {
			h.run.lockAssignee = func(string, string) (func(), error) { return nil, errors.New("injected") }
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{})
			tc.inject(h)
			_ = h.sling(slingBead, "gastown")
			if tc.clears {
				h.wantCalls("clear orphan labels", "clear orphan labels "+slingBead)
			} else {
				h.wantNo("clear orphan labels")
			}
		})
	}
}

// TestSlingDuplicateContent (gt-mcq): live work sharing a test refuses the
// sling before the target is resolved; --force skips the check, and a dry
// run reports the refusal without refusing.
func TestSlingDuplicateContent(t *testing.T) {
	t.Parallel()
	dup := []duplicateMatch{{Bead: duplicateCandidate{ID: "gt-other", Status: "open"}, SharedTests: []string{"TestX"}}}
	for _, tc := range []struct {
		name          string
		force, dryRun bool
		refused       bool
	}{
		{name: "refused", refused: true},
		{name: "force", force: true},
		{name: "dry run", dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{})
			h.run.opts.force, h.run.opts.dryRun = tc.force, tc.dryRun
			checked := false
			h.run.checkDuplicates = func(string, string, *beadInfo) (*duplicateCandidate, []duplicateMatch, error) {
				checked = true
				return nil, dup, nil
			}
			err := h.sling(slingBead, "gastown")
			if checked == tc.force {
				t.Errorf("duplicate check ran = %v under --force=%v", checked, tc.force)
			}
			if tc.refused {
				wantSlingErr(t, err, "Refusing to sling")
				h.wantNo("resolve target")
				return
			}
			if err != nil {
				t.Fatalf("sling: %v", err)
			}
		})
	}
}

// TestSlingBatchGetsTheResolvedRequest: the batch paths get the run's
// options as resolved, so --stdin's instructions and the branch --pr names
// reach every bead of a batch, not the raw flags.
func TestSlingBatchGetsTheResolvedRequest(t *testing.T) {
	t.Parallel()
	for _, deferred := range []bool{false, true} {
		h := newSlingHarness(t)
		h.run.shouldDefer = func() (bool, error) { return deferred, nil }
		h.run.opts.stdin = true
		h.run.stdin = strings.NewReader("do the thing\n")
		h.run.opts.resumePR = 42
		h.run.resolvePRBranch = func(int) (string, error) { return "feature/pr-42", nil }
		if err := h.sling("gt-a", "gt-b", "gt-c", "gastown"); err != nil {
			t.Fatalf("deferred=%v: sling: %v", deferred, err)
		}
		if h.batchOpts.argsText != "do the thing" || h.batchOpts.resumeBranch != "feature/pr-42" {
			t.Errorf("deferred=%v: batch got args %q, resume branch %q", deferred, h.batchOpts.argsText, h.batchOpts.resumeBranch)
		}
	}
}
