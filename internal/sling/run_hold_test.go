package sling

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// holdMidDispatch is the incident of gt-0k7kb: an automatic dispatcher read a
// bead, found it clean and started the sling, and a steward wrote the hold in
// the seconds the sling spent spawning a polecat. The dispatch's guards had
// all run before that write and never read the human hold labels at all, so
// the hook write below overwrote the steward's status and hooked work the town
// had reserved for a person.
//
// These tests drive Run itself, since the read has to sit at the claim — after
// the spawn, with nothing but the hook write left — and no earlier guard can
// stand in for it.
const (
	holdTestTownRoot = "/town"
	holdTestBeadID   = "gt-hold"
)

// holdTestRead is one read of the bead: what a success returns, or the error
// it fails with. A dispatch that changes its mind about a bead mid-flight is
// a sequence of these.
type holdTestRead struct {
	bead *Bead
	err  error
}

// fakeSlingDeps is the smallest Deps a dispatch that reaches the claim needs:
// every collaborator up to the hook write, plus no-ops for the steps after it
// so the tests that let a dispatch through can run to the end.
type fakeSlingDeps struct {
	reads     []holdTestRead // consumed in order; the last entry repeats
	agentDead bool           // what AgentDead answers, for the auto-force path
	hooks     int
	rollback  int
	started   int
}

func (f *fakeSlingDeps) deps() *Deps {
	info := func(string, string) (*Bead, error) {
		if len(f.reads) == 0 {
			return &Bead{Status: "open"}, nil
		}
		r := f.reads[0]
		if len(f.reads) > 1 {
			f.reads = f.reads[1:]
		}
		return r.bead, r.err
	}
	return &Deps{
		Out:            io.Discard,
		LockBead:       func(string, string) (func(), error) { return func() {}, nil },
		BeadInfoInTown: info,
		Actor:          func(Options) string { return "daemon/spec-dispatch" },
		Requester:      func() string { return "daemon/spec-dispatch" },
		SpawnPolecat: func(string, SpawnOptions) (*Spawn, error) {
			return &Spawn{RigName: "gastown", PolecatName: "onyx"}, nil
		},
		LockAssignee:       func(string, string) (func(), error) { return func() {}, nil },
		AgentDead:          func(string) bool { return f.agentDead },
		SurvivingWorkGuard: func(string, string, string) error { return nil },
		ClearReassigned:    func(string, string) {},
		HookDir:            func(string, string, string) string { return "/town/hook" },
		Hook:               func(string, string, string, string) error { f.hooks++; return nil },
		RecordReassignment: func(string, string, string, string, string) {},
		ClearOrphanLabels:  func(string, string, string) {},
		NoteDispatched:     func(string, *Duplicate) {},
		LogFeed:            func(string, string, map[string]interface{}) error { return nil },
		UpdateAgentHook:    func(string, string, string, string) {},
		UpdateAgentMode:    func(string, string, string, string) {},
		StoreFields:        func(string, string, FieldUpdates) error { return nil },
		StartSession:       func(*Spawn) (string, error) { f.started++; return "%1", nil },
		RollbackArtifacts: func(*Spawn, string, string, string) {
			f.rollback++
		},
		RestoreRawFields: func(string, string, string, *Bead) {},
		ReleaseSeat:      func(*Spawn) {},
	}
}

func holdTestOptions() Options {
	return Options{
		BeadID:      holdTestBeadID,
		TownRoot:    holdTestTownRoot,
		Agent:       "deepseek-flash",
		HookRawBead: true,
		// The content-overlap check counts open work in the rig, which is
		// another dispatch's question; these tests are about the claim read.
		SkipDuplicateCheck: true,
	}
}

// TestRunRefusesAHoldWrittenMidDispatch is the regression test: the hold
// appears after the dispatch's first read of the bead — the read the guards
// above run on — and the dispatch must give the bead back rather than hook it.
func TestRunRefusesAHoldWrittenMidDispatch(t *testing.T) {
	t.Parallel()
	clean := &Bead{Status: "open"}
	held := &Bead{Status: "open", Labels: []string{"gt:task", "needs-human"}}
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: clean}, {bead: held}}}

	result, err := Run(context.Background(), f.deps(), holdTestOptions())
	if err == nil {
		t.Fatalf("the dispatch took a bead held after it started (result %+v)", result)
	}
	if !strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
		t.Errorf("err = %v, want the refusal marker %q: an automatic dispatcher must read this as a deferral, not a failed dispatch",
			err, dispatch.SlingRefusalMarker)
	}
	if !strings.Contains(err.Error(), "needs-human") {
		t.Errorf("err = %v, want the hold's own marker named", err)
	}
	if f.hooks != 0 {
		t.Errorf("the bead was hooked %d time(s), want 0", f.hooks)
	}
	if f.started != 0 {
		t.Errorf("a session was started %d time(s), want 0", f.started)
	}
	if f.rollback != 1 {
		t.Errorf("rolled back %d spawned polecat(s), want 1: a refusal at the claim is past the spawn", f.rollback)
	}
}

// TestRunRefusesAHoldItCannotRead: the hold that lands mid-dispatch is
// exactly the one a lagging read can catch, so a re-read that fails is a
// deferral too. Unknown is not free — taking the bead blind is what the
// steward's hold exists to prevent.
func TestRunRefusesAHoldItCannotRead(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{reads: []holdTestRead{
		{bead: &Bead{Status: "open"}},
		{err: errors.New("bd show: connection refused")},
	}}

	_, err := Run(context.Background(), f.deps(), holdTestOptions())
	if err == nil || !strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
		t.Fatalf("err = %v, want a refusal carrying %q", err, dispatch.SlingRefusalMarker)
	}
	if f.hooks != 0 {
		t.Errorf("the bead was hooked %d time(s), want 0", f.hooks)
	}
}

// TestRunTakesABeadRoutedByItsOwnSeat: needs-pro is the pro seat's selector,
// and a sling is already given its target, so the routing label is not a hold
// at the claim — refusing it there would break the pro seat's own dispatch.
func TestRunTakesABeadRoutedByItsOwnSeat(t *testing.T) {
	t.Parallel()
	open := &Bead{Status: "open", Labels: []string{"needs-pro"}}
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: open}, {bead: open}}}

	result, err := Run(context.Background(), f.deps(), holdTestOptions())
	if err != nil {
		t.Fatalf("a needs-pro bead was refused at the claim: %v", err)
	}
	if f.hooks != 1 || result == nil || !result.Success {
		t.Fatalf("hooks = %d, result %+v, want the dispatch to take the bead", f.hooks, result)
	}
}

// TestRunForceTakesAHeldBead: the operator's own --force is the override the
// documented guards promise (docs/concepts/dispatch-holds.md), and this read
// answers to the same flag rather than inventing a second policy.
func TestRunForceTakesAHeldBead(t *testing.T) {
	t.Parallel()
	held := &Bead{Status: "open", Labels: []string{"needs-human"}}
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: held}}}

	opts := holdTestOptions()
	opts.Force = true
	if _, err := Run(context.Background(), f.deps(), opts); err != nil {
		t.Fatalf("--force did not override the hold: %v", err)
	}
	if f.hooks != 1 {
		t.Errorf("hooks = %d, want the forced dispatch to take the bead", f.hooks)
	}
}

// TestRunRefusesABeadDeferredMidDispatch: the status is half of the hold a
// steward writes (deferred, plus the label), and a status alone lands in the
// same window. The claim read runs the same deferred test the guard at the
// start ran, so the half that arrived with no label is caught too.
func TestRunRefusesABeadDeferredMidDispatch(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{reads: []holdTestRead{
		{bead: &Bead{Status: "open"}},
		{bead: &Bead{Status: "deferred"}},
	}}

	_, err := Run(context.Background(), f.deps(), holdTestOptions())
	if err == nil || !strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
		t.Fatalf("err = %v, want a refusal carrying %q", err, dispatch.SlingRefusalMarker)
	}
	if !strings.Contains(err.Error(), "deferred") {
		t.Errorf("err = %v, want the hold named", err)
	}
	if f.hooks != 0 {
		t.Errorf("the bead was hooked %d time(s), want 0", f.hooks)
	}
}

// TestRunAutoForceStillTakesAHookedBead: the dead-agent auto-force re-slings a
// bead whose holder died, and the bead's status is still hooked when the claim
// is reached. Reading the status there would refuse every recovery the
// auto-force exists for, so the claim read leaves it to the guards at the
// start.
func TestRunAutoForceStillTakesAHookedBead(t *testing.T) {
	t.Parallel()
	dead := &Bead{Status: "hooked", Assignee: "gastown/polecats/ghost"}
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: dead}, {bead: dead}}}
	f.agentDead = true

	result, err := Run(context.Background(), f.deps(), holdTestOptions())
	if err != nil {
		t.Fatalf("a bead whose dead holder was auto-forced was refused: %v", err)
	}
	if f.hooks != 1 || result == nil || !result.Success {
		t.Fatalf("hooks = %d, result %+v, want the recovering dispatch to take the bead", f.hooks, result)
	}
}
