package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sling"
)

// TestEngineDepsWiresEveryCollaborator is the wiring guard for slingEngineDeps.
//
// The daemon is handed these Deps at startup and cannot complete or check them
// itself — internal/daemon cannot import this package — so a collaborator added
// to sling.Deps and left unwired here reaches the daemon as a nil func and
// panics on the first convoy feed, in a background process, far from the
// omission. Building the production Deps and requiring every field to be set
// reports it as a test failure instead (gt-638go.7).
func TestEngineDepsWiresEveryCollaborator(t *testing.T) {
	t.Parallel()

	// The guard has to be able to fail: an engine with nothing wired is the
	// input it exists to catch, and every collaborator in the engine is one of
	// the fields it looks at.
	if empty := unwiredEngineDeps(&sling.Deps{}); len(empty) == 0 {
		t.Fatal("the wiring guard sees no unwired fields in an empty sling.Deps, so it cannot fail")
	}

	deps := slingEngineDeps()
	if deps == nil {
		t.Fatal("slingEngineDeps() returned nil")
	}
	for _, name := range unwiredEngineDeps(deps) {
		t.Errorf("sling.Deps.%s is not wired: the daemon's convoy feeder reaches it as nil", name)
	}
}

// unwiredEngineDeps names the collaborators of deps that are nil — the fields a
// dispatch would panic on the first time it reached them.
func unwiredEngineDeps(deps *sling.Deps) []string {
	var unwired []string
	v := reflect.ValueOf(deps).Elem()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		value := v.Field(i)
		switch value.Kind() {
		case reflect.Func, reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice:
			if value.IsNil() {
				unwired = append(unwired, typ.Field(i).Name)
			}
		}
	}
	return unwired
}

// TestEngineSpawnCarriesTheCmdRecord: engineSpawn is the engine's view of a
// spawn this package made, and the record it carries is what every mechanism
// the engine calls back into reaches its state through. A missing or wrong Ref
// is not a compile error, so it is checked here.
func TestEngineSpawnCarriesTheCmdRecord(t *testing.T) {
	t.Parallel()
	cmd := &SpawnedPolecatInfo{
		RigName:     "gastown",
		PolecatName: "Toast",
		ClonePath:   "/town/gastown/polecats/Toast",
		BaseBranch:  "main",
		account:     "acct",
		agent:       "claude",
	}

	spawn := engineSpawn(cmd)
	if spawn.RigName != cmd.RigName || spawn.PolecatName != cmd.PolecatName ||
		spawn.ClonePath != cmd.ClonePath || spawn.BaseBranch != cmd.BaseBranch {
		t.Errorf("spawn = %+v, want the cmd record's fields", spawn)
	}
	if spawn.Ref != cmd {
		t.Errorf("spawn.Ref = %v, want the SpawnedPolecatInfo it came from", spawn.Ref)
	}
	if spawn.OriginalHold != nil {
		t.Errorf("spawn.OriginalHold = %+v, want nil for a spawn with no recorded hold", spawn.OriginalHold)
	}

	// A recorded hold crosses the boundary: the engine's rollback hands the
	// bead back to that holder.
	cmd.originalHold = &beadHold{Status: "hooked", Assignee: "gastown/polecats/Nux"}
	spawn = engineSpawn(cmd)
	if spawn.OriginalHold == nil || spawn.OriginalHold.Status != "hooked" ||
		spawn.OriginalHold.Assignee != "gastown/polecats/Nux" {
		t.Errorf("spawn.OriginalHold = %+v, want the recorded hold", spawn.OriginalHold)
	}
}

// TestCmdSpawnRoundTripsThisPackagesRecord: the engine hands a spawn back to
// the mechanisms the engine's Deps point at, and each of them needs this
// package's record rather than the engine's reduced view of it.
func TestCmdSpawnRoundTripsThisPackagesRecord(t *testing.T) {
	t.Parallel()
	cmd := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", account: "acct", agent: "claude"}
	spawn := engineSpawn(cmd)
	spawn.OriginalHold = &sling.Hold{Status: "open", Assignee: ""}

	if got := cmdSpawn(spawn); got != cmd {
		t.Fatalf("cmdSpawn = %p, want the record engineSpawn was given (%p)", got, cmd)
	}
	if cmd.originalHold == nil || cmd.originalHold.Status != "open" {
		t.Errorf("cmdSpawn left originalHold = %+v, want the engine's hold on the record", cmd.originalHold)
	}
}

// TestCmdSpawnPanicsOnAForeignSpawn: only a spawn this package made can be
// started or rolled back, and the mechanisms that take one would otherwise
// dereference a nil record somewhere further from the cause.
func TestCmdSpawnPanicsOnAForeignSpawn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		spawn *sling.Spawn
		want  string
	}{
		{name: "a spawn record the engine made itself", spawn: &sling.Spawn{PolecatName: "Toast"}, want: "carries no polecat record"},
		{name: "a spawn whose Ref is another type", spawn: &sling.Spawn{PolecatName: "Toast", Ref: "not a polecat"}, want: "carries no polecat record"},
		{name: "a spawn whose Ref is a nil record", spawn: &sling.Spawn{PolecatName: "Toast", Ref: (*SpawnedPolecatInfo)(nil)}, want: "carries no polecat record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("cmdSpawn(%+v) did not panic", tc.spawn)
				}
				if msg, _ := r.(string); !strings.Contains(msg, tc.want) {
					t.Errorf("panic = %q, want it to name %q", r, tc.want)
				}
			}()
			cmdSpawn(tc.spawn)
		})
	}
}

// TestEngineReleaseSeatDropsThisProcessesSpawnSeat: the engine's release runs
// with the spawn the dispatch made, and drops the pool seat that spawn
// reserved. A spawn the engine made itself has no seat in this process, so the
// release must not reach for a record it does not have.
func TestEngineReleaseSeatDropsThisProcessesSpawnSeat(t *testing.T) {
	t.Parallel()
	d := &slingDeps{}

	var released []*SpawnedPolecatInfo
	d.releaseSeat = func(s *SpawnedPolecatInfo) { released = append(released, s) }

	// The boundary runs on every dispatch, including the ones that made no
	// spawn: it releases nothing rather than a seat that belongs to someone
	// else.
	d.engineReleaseSeat(nil)
	if len(released) != 1 || released[0] != nil {
		t.Fatalf("release with no spawn = %v, want one release of nil", released)
	}

	released = nil
	d.engineReleaseSeat(&sling.Spawn{PolecatName: "Toast"}) // no Ref: not ours
	if len(released) != 1 || released[0] != nil {
		t.Fatalf("release of a spawn the engine made itself = %v, want one release of nil", released)
	}

	released = nil
	cmd := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast"}
	d.engineReleaseSeat(engineSpawn(cmd))
	if len(released) != 1 || released[0] != cmd {
		t.Fatalf("release = %v, want the spawn's own record %p", released, cmd)
	}
}

// TestEngineDefaultFormulaIsTheRigsDefault: the engine asks the cobra layer for
// the formula a dispatch that named none runs under, and gets what `gt sling`
// would have applied — resolveFormula's answer, not an empty string that would
// hook the raw bead.
func TestEngineDefaultFormulaIsTheRigsDefault(t *testing.T) {
	t.Parallel()
	d := &slingDeps{
		resolveFormula: func(explicit string, hookRaw bool, _, rigName string) string {
			if explicit != "" || hookRaw {
				t.Errorf("default lookup asked with explicit=%q hookRaw=%v, want the plain default", explicit, hookRaw)
			}
			return "mol-rig-" + rigName
		},
	}
	if got, want := d.engineDefaultFormula("/town", "gastown"), "mol-rig-gastown"; got != want {
		t.Errorf("engineDefaultFormula = %q, want %q", got, want)
	}
}
