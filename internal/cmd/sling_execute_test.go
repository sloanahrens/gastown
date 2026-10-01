package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// executeSling is the batch, convoy and scheduler dispatch path. These tests
// run it on slingHarness fakes.

func executeParams() SlingParams {
	return SlingParams{BeadID: slingBead, RigName: "gastown", TownRoot: slingTestTown, NoConvoy: true, FormulaFailFatal: true}
}

func TestExecuteSlingRefusals(t *testing.T) {
	t.Parallel()
	const holder = "gastown/polecats/Nux"
	cases := []struct {
		name   string
		bead   *beadInfo // nil = absent from the world
		params func(p *SlingParams)
		setup  func(h *slingHarness)
		errSub string
		errMsg string // SlingResult.ErrMsg
	}{
		{name: "missing bead", errSub: "could not get bead info"},
		// The per-bead lock comes before the status read (TOCTOU).
		{name: "bead lock busy", bead: &beadInfo{}, setup: func(h *slingHarness) {
			h.run.lockBead = func(string, string) (func(), error) { return nil, errors.New("bead gt-abc123 is already being slung") }
			h.run.beadInfoInTown = func(string, string) (*beadInfo, error) {
				h.t.Error("bead read before its lock was held")
				return nil, errors.New("unreachable")
			}
		}, errSub: "already being slung"},
		{name: "closed", bead: &beadInfo{Status: "closed"}, errSub: "work already completed", errMsg: "already closed"},
		{name: "tombstone under force", bead: &beadInfo{Status: "tombstone"}, params: func(p *SlingParams) { p.Force = true }, errSub: "is tombstone"},
		{name: "hooked to a live agent", bead: &beadInfo{Status: "hooked", Assignee: holder}, errSub: "already hooked", errMsg: "already hooked"},
		{name: "deferred", bead: &beadInfo{Status: "deferred"}, errSub: "is deferred", errMsg: "deferred"},
		// A dead holder auto-forces, but only an explicit --force passes the
		// deferred gate.
		{name: "deferred with a dead holder", bead: &beadInfo{Status: "hooked", Assignee: holder, Description: "status: deferred"},
			setup: func(h *slingHarness) { h.dead[holder] = true }, errSub: "is deferred"},
		{name: "operator", bead: &beadInfo{Labels: []string{dispatch.OperatorLabel}}, errSub: dispatch.SlingRefusalMarker, errMsg: "operator-reserved"},
		// An e-stop refuses every sling, explicit ones included (gt-4k3fj.4),
		// and wins over a parked rig.
		{name: "town e-stop", bead: &beadInfo{}, setup: func(h *slingHarness) {
			h.estops[""] = true
			h.run.rigParked = func(string, string) (bool, string) { return true, "parked" }
		}, errSub: "gt thaw", errMsg: "e-stop"},
		{name: "rig e-stop", bead: &beadInfo{}, setup: func(h *slingHarness) { h.estops["gastown"] = true },
			errSub: "gt thaw --rig gastown", errMsg: "e-stop"},
		{name: "parked rig", bead: &beadInfo{}, setup: func(h *slingHarness) {
			h.run.rigParked = func(string, string) (bool, string) { return true, "parked" }
		}, errSub: "gt rig unpark gastown", errMsg: "rig parked"},
		{name: "docked rig", bead: &beadInfo{}, setup: func(h *slingHarness) {
			h.run.rigParked = func(string, string) (bool, string) { return true, "docked" }
		}, errSub: "gt rig undock gastown"},
		{name: "not in the target rig database", bead: &beadInfo{}, setup: func(h *slingHarness) {
			h.run.verifyInTargetRig = func(id, rig, _ string) error { return errors.New("bead is not present in target rig") }
		}, errSub: "not present in target rig"},
		{name: "dead holder whose work survives", bead: &beadInfo{Status: "hooked", Assignee: holder}, setup: func(h *slingHarness) {
			h.dead[holder] = true
			h.run.survivingWorkGuard = func(string, string, string) error { return &reslingRefusal{msg: "work survives"} }
		}, errSub: "work survives"},
		{name: "live molecule", bead: &beadInfo{Status: "blocked"}, params: func(p *SlingParams) { p.FormulaName = "mol-polecat-work" },
			setup: func(h *slingHarness) { h.molecules[slingBead] = []string{"gt-wisp-old"} }, errSub: "has existing molecule(s)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			if tc.bead != nil {
				h.addBead(slingBead, *tc.bead)
			}
			if tc.setup != nil {
				tc.setup(h)
			}
			p := executeParams()
			if tc.params != nil {
				tc.params(&p)
			}
			res, err := h.run.executeSling(p)
			wantSlingErr(t, err, tc.errSub)
			if res == nil || res.Success {
				t.Fatalf("result = %+v, want a failed result", res)
			}
			if tc.errMsg != "" && res.ErrMsg != tc.errMsg {
				t.Errorf("ErrMsg = %q, want %q", res.ErrMsg, tc.errMsg)
			}
			h.wantNo("spawn")
			h.wantNo("hook")
			// The bead lock and the pool seat never outlive the dispatch.
			if len(h.matching("lock bead")) != len(h.matching("unlock bead")) {
				t.Errorf("bead lock not released: %q", h.log())
			}
			h.wantCalls("release seat", "release seat")
		})
	}
}

// Another rig's e-stop does not hold this one.
func TestExecuteSlingOtherRigEstopDoesNotRefuse(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{})
	h.estops["om"] = true
	if _, err := h.run.executeSling(executeParams()); err != nil {
		t.Fatalf("executeSling into gastown with only om e-stopped: %v", err)
	}
	h.wantCalls("spawn", "spawn gastown")
}

// A rig target is refused under its rig's e-stop before any polecat spawns.
func TestResolveSlingTargetRigEstopRefusesBeforeSpawn(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.estops["gastown"] = true
	_, err := h.run.resolveSlingTarget("gastown", ResolveTargetOptions{TownRoot: slingTestTown, NoBoot: true})
	wantSlingErr(t, err, "E-stop active")
	h.wantNo("spawn")
	h.wantNo("admit")
}

func TestExecuteSlingSuccess(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{})
	p := executeParams()
	p.FormulaName, p.NoConvoy, p.Mode, p.Vars = "mol-polecat-work", false, "ralph", []string{"k=v"}
	res, err := h.run.executeSling(p)
	if err != nil {
		t.Fatalf("executeSling: %v", err)
	}
	if !res.Success || res.PolecatName != "Toast" || res.AttachedMolecule != "gt-wisp-new" {
		t.Errorf("result = %+v", res)
	}
	want := []string{
		"lock bead gt-abc123",
		"spawn gastown",
		"convoy lookup gt-abc123",
		"create convoy gt-abc123",
		"cook mol-polecat-work",
		"instantiate mol-polecat-work on gt-abc123 vars=k=v",
		"lock assignee gastown/polecats/Toast",
		"reassign gt-abc123  -> gastown/polecats/Toast",
		"hook gt-abc123 gastown/polecats/Toast",
		"clear orphan labels gt-abc123",
		"feed sling",
		"agent hook gastown/polecats/Toast gt-abc123",
		"store fields gt-abc123",
		"agent mode gastown/polecats/Toast ralph",
		"start session Toast",
		"unlock assignee gastown/polecats/Toast",
		"unlock bead gt-abc123",
		"release seat",
	}
	if got := h.log(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("call log:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestExecuteSlingForceStealsFromALivePolecat: the old polecat's agent state
// is cleared before the new spawn.
func TestExecuteSlingForceStealsFromALivePolecat(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: "gastown/polecats/Nux"})
	p := executeParams()
	p.Force = true
	if _, err := h.run.executeSling(p); err != nil {
		t.Fatalf("executeSling: %v", err)
	}
	h.wantCalls("clear reassigned", "clear reassigned gastown/polecats/Nux")
	h.wantCalls("reassign", "reassign gt-abc123 gastown/polecats/Nux -> gastown/polecats/Toast")
	if log := strings.Join(h.log(), "\n"); strings.Index(log, "clear reassigned") > strings.Index(log, "spawn gastown") {
		t.Errorf("spawned before the old holder was cleared:\n%s", log)
	}
}

// TestExecuteSlingDeadHolder: a dead holder is auto-forced once the survival
// guard finds no work to protect; its stale molecules are burned.
func TestExecuteSlingDeadHolder(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{Status: "hooked", Assignee: "gastown/polecats/Nux"})
	h.dead["gastown/polecats/Nux"] = true
	h.molecules[slingBead] = []string{"gt-wisp-old"}
	p := executeParams()
	p.FormulaName = "mol-polecat-work"
	if _, err := h.run.executeSling(p); err != nil {
		t.Fatalf("executeSling: %v", err)
	}
	h.wantCalls("survival guard", "survival guard gt-abc123 gastown/polecats/Nux")
	h.wantCalls("burn", "burn gt-abc123 gt-wisp-old")
}

// TestExecuteSlingRollsBackAfterTheSpawn: a failure after the spawn undoes
// it; a formula failure is fatal only when the caller says so (batch hooks
// the raw bead instead).
func TestExecuteSlingRollsBackAfterTheSpawn(t *testing.T) {
	t.Parallel()
	injected := errors.New("injected failure")
	cases := []struct {
		name    string
		params  func(p *SlingParams)
		inject  func(h *slingHarness)
		errSub  string // "" = success
		undo    string // the undo call
		restore bool   // raw workflow fields restored
	}{
		{name: "cook fails", params: func(p *SlingParams) { p.FormulaName = "mol-polecat-work" },
			inject: func(h *slingHarness) { h.run.cook = func(string, string, string) error { return injected } },
			errSub: "cooking formula", undo: "rollback Toast bead=gt-abc123 convoy=", restore: true},
		{name: "formula fails", params: func(p *SlingParams) { p.FormulaName = "mol-polecat-work" },
			inject: func(h *slingHarness) {
				h.run.instantiateFormula = func(context.Context, string, string, string, string, string, []string) (*FormulaOnBeadResult, error) {
					return nil, injected
				}
			},
			errSub: "instantiating formula", undo: "rollback Toast bead=gt-abc123 convoy=", restore: true},
		{name: "formula fails in a batch", params: func(p *SlingParams) { p.FormulaName, p.FormulaFailFatal = "mol-polecat-work", false },
			inject: func(h *slingHarness) {
				h.run.instantiateFormula = func(context.Context, string, string, string, string, string, []string) (*FormulaOnBeadResult, error) {
					return nil, injected
				}
			}},
		{name: "assignee lock fails", inject: func(h *slingHarness) {
			h.run.lockAssignee = func(string, string) (func(), error) { return nil, injected }
		}, errSub: "serializing hook write", undo: "cleanup spawn Toast convoy="},
		{name: "raw metadata fails", params: func(p *SlingParams) { p.ReviewOnly = true },
			inject: func(h *slingHarness) {
				h.run.storeFields = func(string, string, beadFieldUpdates) error { return injected }
			},
			errSub: "storing raw sling metadata", undo: "cleanup spawn Toast convoy=", restore: true},
		{name: "hook fails", inject: func(h *slingHarness) {
			h.run.hook = func(string, string, string, string) error { return injected }
		}, errSub: "failed to hook bead", undo: "rollback Toast bead=gt-abc123 convoy=", restore: true},
		{name: "session fails", inject: func(h *slingHarness) {
			h.run.startSession = func(*SpawnedPolecatInfo) (string, error) { return "", injected }
		}, errSub: "starting polecat session", undo: "rollback Toast bead=gt-abc123 convoy=", restore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{})
			tc.inject(h)
			p := executeParams()
			if tc.params != nil {
				tc.params(&p)
			}
			res, err := h.run.executeSling(p)
			if tc.errSub == "" {
				if err != nil || !res.Success {
					t.Fatalf("executeSling = %+v, %v; want success", res, err)
				}
				h.wantNo("rollback")
				h.wantCalls("hook", "hook gt-abc123 gastown/polecats/Toast")
				return
			}
			wantSlingErr(t, err, tc.errSub)
			var undo []string
			undo = append(undo, h.matching("rollback")...)
			undo = append(undo, h.matching("cleanup spawn")...)
			if len(undo) != 1 || undo[0] != tc.undo {
				t.Errorf("undo calls = %q, want exactly %q", undo, tc.undo)
			}
			if got := len(h.matching("restore raw fields")) == 1; got != tc.restore {
				t.Errorf("raw fields restored = %v, want %v", got, tc.restore)
			}
		})
	}
}

// TestExecuteSlingDuplicateContent (gt-mcq): a blocking overlap is refused
// with the duplicate error kind; SkipDuplicateCheck and Force skip the check.
func TestExecuteSlingDuplicateContent(t *testing.T) {
	t.Parallel()
	dup := []duplicateMatch{{Bead: duplicateCandidate{ID: "gt-other", Status: "hooked"}, SharedTests: []string{"TestX"}}}
	for _, tc := range []struct {
		name    string
		params  func(p *SlingParams)
		refused bool
	}{
		{"refused", func(*SlingParams) {}, true},
		{"force", func(p *SlingParams) { p.Force = true }, false},
		{"skip", func(p *SlingParams) { p.SkipDuplicateCheck = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{})
			h.run.checkDuplicates = func(string, string, *beadInfo) (*duplicateCandidate, []duplicateMatch, error) {
				return nil, dup, nil
			}
			p := executeParams()
			tc.params(&p)
			res, err := h.run.executeSling(p)
			if !tc.refused {
				if err != nil {
					t.Fatalf("executeSling: %v", err)
				}
				return
			}
			wantSlingErr(t, err, "Refusing to sling")
			if res.ErrMsg != errSlingDuplicateContent.Error() {
				t.Errorf("ErrMsg = %q, want %q", res.ErrMsg, errSlingDuplicateContent)
			}
			h.wantNo("spawn")
		})
	}
}
