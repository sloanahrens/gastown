package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sling"
)

// The feeder's in-process dispatch is the one place internal/daemon reaches
// the engine, and every other test of it stubs slingFn. These drive the real
// slingInProcess over a real sling.Deps whose collaborators are recording
// fakes, so the wiring between the two — which Options the feeder hands over
// and which collaborator sees them — is exercised rather than assumed
// (gt-638go.7).

// fakeEngineDeps is a sling.Deps that records what a dispatch through it
// reached, and stops at the step its stop function names.
type fakeEngineDeps struct {
	calls   []string
	bead    *sling.Bead
	beadErr error
	// stopAt is the collaborator name that returns an error, ending the
	// dispatch at that step. Empty runs to the spawn, which the other
	// collaborators then refuse.
	stopAt string
}

func (f *fakeEngineDeps) record(name string) bool {
	f.calls = append(f.calls, name)
	return f.stopAt == name
}

func (f *fakeEngineDeps) reached(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (f *fakeEngineDeps) deps() *sling.Deps {
	return &sling.Deps{
		FindTown:    func() (string, error) { return "/town", nil },
		Actor:       func(sling.Options) string { return "daemon/convoy:hq-cv-1" },
		Requester:   func() string { return "daemon" },
		ReleaseSeat: func(*sling.Spawn) {},
		EstopOn:     func(string, string) (bool, error) { return false, nil },
		RigParked:   func(string, string) (bool, string) { return false, "" },
		BeadInfoInTown: func(_, _ string) (*sling.Bead, error) {
			f.record("bead-info")
			if f.beadErr != nil {
				return nil, f.beadErr
			}
			if f.bead == nil {
				return nil, errors.New("no bead staged")
			}
			return f.bead, nil
		},
		VerifyInTargetRig: func(_, _, _ string) error {
			f.record("verify-rig")
			return nil
		},
		LockBead: func(string, string) (func(), error) {
			f.record("lock-bead")
			return func() { f.record("unlock-bead") }, nil
		},
		DefaultFormula: func(_, rigName string) string {
			f.calls = append(f.calls, "default-formula:"+rigName)
			return "mol-rig-default"
		},
		CollectMolecules: func(_ *sling.Bead, beadID, _ string) ([]string, error) {
			f.calls = append(f.calls, "collect-molecules:"+beadID)
			if f.stopAt == "collect-molecules" {
				return nil, errors.New("stopped at collect-molecules")
			}
			return nil, nil
		},
		SpawnPolecat: func(string, sling.SpawnOptions) (*sling.Spawn, error) {
			f.record("spawn")
			return nil, errors.New("the feeder test spawns nothing")
		},
	}
}

// TestSlingInProcessRunsTheEngineOverTheDaemonDeps: the feeder's real dispatch
// path enters sling.Run with the Options it built, and the engine's first
// collaborators are the daemon's own.
func TestSlingInProcessRunsTheEngineOverTheDaemonDeps(t *testing.T) {
	t.Parallel()
	f := &fakeEngineDeps{
		bead:   &sling.Bead{Title: "feed me", Status: "open"},
		stopAt: "collect-molecules",
	}
	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, f.deps(), 0, nil, nil, nil)

	_, err := m.slingInProcess("hq-cv-1", sling.Options{
		BeadID:             "gt-issue1",
		RigName:            "gastown",
		NoBoot:             true,
		Actor:              "daemon/convoy:hq-cv-1",
		TownRoot:           m.townRoot,
		SkipDuplicateCheck: true,
	})
	if err == nil || !strings.Contains(err.Error(), "collect-molecules") {
		t.Fatalf("slingInProcess err = %v, want the dispatch stopped at collect-molecules", err)
	}

	for _, want := range []string{"lock-bead", "bead-info", "verify-rig", "default-formula:gastown", "collect-molecules:gt-issue1"} {
		if !f.reached(want) {
			t.Errorf("the dispatch never reached %s; calls = %v", want, f.calls)
		}
	}
	if !f.reached("unlock-bead") {
		t.Errorf("the bead lock outlived the dispatch; calls = %v", f.calls)
	}
}

// TestSlingInProcessDefaultsTheFormulaLikeTheCLIItReplaced: the convoy feeder
// hands over the formula the convoy recorded, which is empty when the original
// sling recorded none. `gt sling`, which this dispatch replaced, resolved the
// rig's default in that case; the engine has to resolve the same one, or the
// re-feed hooks a raw bead where the command it replaced ran a formula
// (gt-4lor).
func TestSlingInProcessDefaultsTheFormulaLikeTheCLIItReplaced(t *testing.T) {
	t.Parallel()
	f := &fakeEngineDeps{
		bead:   &sling.Bead{Title: "no formula recorded", Status: "open"},
		stopAt: "collect-molecules",
	}
	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, f.deps(), 0, nil, nil, nil)

	// The formula the convager feeder records on a convoy: the one the original
	// sling asked for, empty when it named none.
	if _, err := m.slingInProcess("hq-cv-1", sling.Options{
		BeadID:             "gt-issue2",
		RigName:            "gastown",
		NoBoot:             true,
		FormulaName:        "",
		TownRoot:           m.townRoot,
		SkipDuplicateCheck: true,
	}); err == nil {
		t.Fatal("slingInProcess succeeded, want the dispatch stopped at collect-molecules")
	}

	if !f.reached("default-formula:gastown") {
		t.Errorf("an unrecorded formula did not resolve the rig's default; calls = %v", f.calls)
	}
	if !f.reached("collect-molecules:gt-issue2") {
		t.Errorf("the defaulted formula did not drive the dispatch; calls = %v", f.calls)
	}
}

// TestSlingInProcessStopsWhenTheDaemonShutsDown: the feeder dispatches under
// the daemon's own context, and Stop cancels it. The dispatch that replaces
// exec'ing `gt sling` has to honor that — the subprocess died by
// CommandContext and process group, and without a context the in-process one
// would run to completion, spawning a polecat into a town that is shutting
// down (gt-638go.7).
func TestSlingInProcessStopsWhenTheDaemonShutsDown(t *testing.T) {
	t.Parallel()
	f := &fakeEngineDeps{bead: &sling.Bead{Title: "shutting down", Status: "open"}}
	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, f.deps(), 0, nil, nil, nil)
	m.cancel() // Stop() cancels the manager's context.

	_, err := m.slingInProcess("hq-cv-1", sling.Options{
		BeadID:             "gt-issue1",
		RigName:            "gastown",
		NoBoot:             true,
		TownRoot:           m.townRoot,
		SkipDuplicateCheck: true,
	})
	if err == nil {
		t.Fatal("a dispatch under a cancelled context ran to completion")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled so the feeder can tell it apart", err)
	}
	if f.reached("spawn") {
		t.Errorf("a cancelled dispatch still spawned a polecat; calls = %v", f.calls)
	}
	if !f.reached("unlock-bead") {
		t.Errorf("the bead lock outlived the cancelled dispatch; calls = %v", f.calls)
	}
}

// TestConvoyFeederRefusesWithoutAnEngine: a daemon constructed with no dispatch
// engine must refuse to feed rather than report a dispatch it never made. This
// is the nil half of the engine's wiring guard — Config.SlingEngine is set by
// the command that starts the daemon, and nothing else checks it (gt-638go.7).
func TestConvoyFeederRefusesWithoutAnEngine(t *testing.T) {
	t.Parallel()
	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, 0, nil, nil, nil)

	if _, err := m.slingInProcess("hq-cv-1", sling.Options{BeadID: "gt-issue1", RigName: "gastown"}); err == nil ||
		!strings.Contains(err.Error(), "no dispatch engine wired") {
		t.Fatalf("slingInProcess with no engine = %v, want a refusal naming the missing engine", err)
	}
	if err := m.slingSlinger()(context.Background(), m.townRoot, sling.Options{BeadID: "gt-issue1", RigName: "gastown"}); err == nil ||
		!strings.Contains(err.Error(), "no dispatch engine wired") {
		t.Fatalf("the continuation feed with no engine = %v, want a refusal naming the missing engine", err)
	}
}
